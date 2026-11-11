package pkgmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/project-copacetic/copacetic/pkg/buildkit"
	"github.com/project-copacetic/copacetic/pkg/types"
	"github.com/project-copacetic/copacetic/pkg/types/unversioned"
	"github.com/project-copacetic/copacetic/pkg/utils"
	log "github.com/sirupsen/logrus"
)

const apkTargetRoot = "/target"

func apkToolingImageNames(osType, osVersion string) (string, string, error) {
	switch utils.CanonicalOSType(osType) {
	case utils.OSTypeAlpine:
		tag, err := alpineToolingTag(osVersion)
		if err != nil {
			return "", "", err
		}
		return imageCachePrefix + "/alpine:" + tag, "docker.io/library/alpine:" + tag, nil
	case utils.OSTypeWolfi:
		return imageCachePrefix + "/chainguard/wolfi-base:latest", "cgr.dev/chainguard/wolfi-base:latest", nil
	default:
		return "", "", fmt.Errorf("external APK tooling is unsupported for OS %q", osType)
	}
}

func alpineToolingTag(osVersion string) (string, error) {
	parts := strings.Split(osVersion, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid Alpine version %q: expected major.minor", osVersion)
	}
	for _, part := range parts[:2] {
		if part == "" {
			return "", fmt.Errorf("invalid Alpine version %q: expected major.minor", osVersion)
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return "", fmt.Errorf("invalid Alpine version %q: expected numeric major.minor", osVersion)
			}
		}
	}
	return parts[0] + "." + parts[1], nil
}

func (am *apkManager) hasAPKExecutable(ctx context.Context, state *llb.State) (bool, error) {
	for _, path := range []string{"/usr/bin/apk", "/sbin/apk"} {
		executable, err := statePathExecutable(ctx, am.config.Client, state, path)
		if err != nil {
			return false, fmt.Errorf("checking %s: %w", path, err)
		}
		if executable {
			return true, nil
		}
	}
	return false, nil
}

func (am *apkManager) externalAPKToolingState(
	ctx context.Context,
	state *llb.State,
	platform *ocispecs.Platform,
) (llb.State, error) {
	cachedImage, upstreamImage, err := apkToolingImageNames(am.osType, am.osVersion)
	if err != nil {
		return llb.State{}, err
	}
	tooling, cachedErr := tryImage(ctx, cachedImage, am.config.Client, platform)
	if cachedErr == nil {
		return tooling, nil
	}
	log.Warnf("Could not resolve cached APK tooling image %s; falling back to %s", cachedImage, upstreamImage)

	tooling, upstreamErr := tryImage(ctx, upstreamImage, am.config.Client, platform)
	if upstreamErr != nil {
		return llb.State{}, fmt.Errorf(
			"failed to resolve APK tooling images for %s/%s: %w",
			platform.OS,
			platform.Architecture,
			errors.Join(cachedErr, upstreamErr),
		)
	}
	return tooling, nil
}

func apkToolArgs(args ...string) []string {
	command := []string{
		"apk",
		"--root", apkTargetRoot,
		"--repositories-file", apkTargetRoot + "/etc/apk/repositories",
		"--keys-dir", apkTargetRoot + "/etc/apk/keys",
	}
	return append(command, args...)
}

func apkToolCommand(args ...string) string {
	return strings.Join(apkToolArgs(args...), " ")
}

func (am *apkManager) upgradePackagesWithExternalAPK(
	ctx context.Context,
	imageStateCurrent *llb.State,
	updates unversioned.UpdatePackages,
	ignoreErrors bool,
) (*llb.State, []byte, error) {
	for _, path := range []string{"/lib/apk/db/installed", "/etc/apk/repositories"} {
		exists, err := stateFileExists(ctx, am.config.Client, imageStateCurrent, path)
		if err != nil {
			return nil, nil, fmt.Errorf("checking APK metadata file %s: %w", path, err)
		}
		if !exists {
			return nil, nil, fmt.Errorf("external APK tooling requires %s in the target image", path)
		}
	}
	keysDirExists, err := statePathExists(ctx, am.config.Client, imageStateCurrent, "/etc/apk/keys")
	if err != nil {
		return nil, nil, fmt.Errorf("checking target APK keys: %w", err)
	}
	if !keysDirExists {
		return nil, nil, fmt.Errorf("external APK tooling requires /etc/apk/keys in the target image")
	}
	shellAvailable, err := statePathExecutable(ctx, am.config.Client, imageStateCurrent, "/bin/sh")
	if err != nil {
		return nil, nil, fmt.Errorf("checking target shell: %w", err)
	}
	if !shellAvailable {
		return nil, nil, fmt.Errorf("external APK tooling requires an executable /bin/sh in the target image to run package scripts")
	}

	platform, err := imageStateCurrent.GetPlatform(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get target image platform: %w", err)
	}
	toolingBase, err := am.externalAPKToolingState(ctx, imageStateCurrent, platform)
	if err != nil {
		return nil, nil, err
	}
	toolingRoot := toolingBase.File(llb.Copy(*imageStateCurrent, "/", apkTargetRoot))

	apkUpdated := toolingRoot.Run(
		llb.Args(apkToolArgs("update")),
		llb.WithProxy(utils.GetProxy()),
		llb.IgnoreCache,
		llb.WithCustomName("Updating package database with external APK tooling"),
		llb.User("root"),
	).Root()

	if updates == nil {
		const updatesAvailableMarker = "/updates.txt"
		listPackages := apkToolCommand("list")
		checkUpgradable := fmt.Sprintf(
			`set -e; %s > /apk-list.txt; if grep -q "upgradable" /apk-list.txt; then touch %s; fi`,
			listPackages,
			updatesAvailableMarker,
		)
		stateWithCheck := apkUpdated.Run(
			buildkit.Sh(checkUpgradable),
			llb.WithCustomName("Checking for available external APK updates"),
			llb.User("root"),
		).Root()

		_, err := buildkit.TryExtractFileFromState(ctx, am.config.Client, &stateWithCheck, updatesAvailableMarker)
		if err != nil {
			if !isMarkerMissingErr(err, updatesAvailableMarker) {
				return nil, nil, fmt.Errorf("failed while checking for available APK updates: %w", err)
			}
			log.Info("No upgradable packages found for this image.")
			return nil, nil, types.ErrNoUpdatesFound
		}
	}

	var apkInstalled llb.State
	var resultManifestBytes []byte
	if updates != nil {
		pkgNames := make([]string, 0, len(updates))
		for _, update := range updates {
			pkgNames = append(pkgNames, update.Name)
		}

		apkAdded := apkUpdated.Run(
			llb.Args(apkToolArgs(append([]string{"add", "--no-cache"}, pkgNames...)...)),
			llb.WithProxy(utils.GetProxy()),
			llb.WithCustomName(fmt.Sprintf("Installing %d security updates with external APK tooling", len(pkgNames))),
			llb.User("root"),
		).Root()
		apkInstalled = apkAdded.Run(
			llb.Args(apkToolArgs(append([]string{"upgrade", "--no-cache"}, pkgNames...)...)),
			llb.WithProxy(utils.GetProxy()),
			llb.WithCustomName(fmt.Sprintf("Upgrading %d security updates with external APK tooling", len(pkgNames))),
			llb.User("root"),
		).Root()

		pkgList := strings.Join(pkgNames, " ")
		resultsCommand := fmt.Sprintf("%s info --installed -v %s > %s", apkToolCommand(), pkgList, resultManifest)
		resultsDiff := apkInstalled.
			File(llb.Mkdir(resultsPath, 0o744, llb.WithParents(true))).
			Dir(resultsPath).
			Run(buildkit.Sh(resultsCommand), llb.WithCustomName("Generating external APK package manifest")).
			AddMount(resultsPath, llb.Scratch())
		resultManifestBytes, err = buildkit.ExtractFileFromState(ctx, am.config.Client, &resultsDiff, resultManifest)
		if err != nil {
			return nil, nil, fmt.Errorf("extracting external APK package manifest: %w", err)
		}
	} else {
		installCommand := fmt.Sprintf(
			`output=$(%s upgrade --no-cache 2>&1); if [ $? -ne 0 ]; then echo "$output" >>error_log.txt; fi`,
			apkToolCommand(),
		)
		apkInstalled = apkUpdated.Run(
			buildkit.Sh(installCommand),
			llb.WithProxy(utils.GetProxy()),
			llb.WithCustomName("Upgrading all packages with external APK tooling"),
			llb.User("root"),
		).Root()
		if !ignoreErrors {
			apkInstalled = apkInstalled.Run(
				buildkit.Sh("if [ -s error_log.txt ]; then cat error_log.txt; exit 1; fi"),
				llb.WithCustomName("Validating external APK package updates"),
				llb.User("root"),
			).Root()
		}
	}

	updatedTarget := llb.Scratch().File(llb.Copy(apkUpdated, apkTargetRoot, "/", &llb.CopyInfo{CopyDirContentsOnly: true}))
	installedTarget := llb.Scratch().File(llb.Copy(apkInstalled, apkTargetRoot, "/", &llb.CopyInfo{CopyDirContentsOnly: true}))
	patchDiff := llb.Diff(updatedTarget, installedTarget)

	if am.config.PatchedConfigData != nil {
		prevPatchDiff := llb.Diff(am.config.ImageState, am.config.PatchedImageState)
		combinedPatch := llb.Merge([]llb.State{prevPatchDiff, patchDiff})
		squashedPatch := llb.Scratch().File(llb.Copy(combinedPatch, "/", "/"))
		completePatchMerge := llb.Merge([]llb.State{am.config.ImageState, squashedPatch})
		return &completePatchMerge, resultManifestBytes, nil
	}

	patchMerge := llb.Merge([]llb.State{am.config.ImageState, patchDiff})
	return &patchMerge, resultManifestBytes, nil
}
