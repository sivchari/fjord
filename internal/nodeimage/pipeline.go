package nodeimage

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/kind"
	"github.com/sivchari/fjord/internal/logger"
)

// buildNodeImageFunc matches internal/kind.BuildNodeImage's signature. It
// exists so tests can substitute a fake that never invokes docker.
type buildNodeImageFunc func(tarballPath, imageTag, arch string, logger logger.Logger) error

// Build downloads the EKS-D server tarball for release/arch (using fjord's
// local cache), rewrites the EKS-D container image tags embedded in it to
// their upstream registry.k8s.io equivalents, and builds a kind node image
// from the result via internal/kind.BuildNodeImage. It returns the image
// tag the node image was built and tagged as (see Tag).
func Build(ctx context.Context, release *eksd.Release, arch string, logger logger.Logger) (string, error) {
	cacheDir, err := DefaultCacheDir()
	if err != nil {
		return "", err
	}

	return build(ctx, release, arch, logger, http.DefaultClient, cacheDir, kind.BuildNodeImage)
}

// build implements Build with every external dependency (the HTTP client,
// the download cache directory, and the node image builder) injected for
// testing.
func build(
	ctx context.Context,
	release *eksd.Release,
	arch string,
	logger logger.Logger,
	client *http.Client,
	cacheDir string,
	buildNodeImage buildNodeImageFunc,
) (string, error) {
	asset, ok := release.ServerTarball[arch]
	if !ok {
		return "", fmt.Errorf("eks version %q has no server tarball for arch %q", release.EKSVersion, arch)
	}

	cachedPath, err := DownloadServerTarball(ctx, client, asset, cacheDir)
	if err != nil {
		return "", err
	}

	rewrittenPath, cleanup, err := RewriteServerTarball(cachedPath)
	if err != nil {
		return "", err
	}

	defer cleanup()

	imageTag := Tag(release)

	if err := buildNodeImage(rewrittenPath, imageTag, arch, logger); err != nil {
		return "", err
	}

	return imageTag, nil
}
