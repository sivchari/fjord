package componentdir

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/eksdcache"
)

// Materialize downloads the EKS-D server tarball for release/arch (using
// fjord's local cache) and extracts it into a rask component directory,
// caching the result so repeated calls do not re-download or re-extract.
// It returns the path to that directory.
func Materialize(ctx context.Context, release *eksd.Release, arch string) (string, error) {
	cacheDir, err := eksdcache.DefaultCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving cache dir: %w", err)
	}

	return materialize(ctx, release, arch, http.DefaultClient, cacheDir)
}

// materialize implements Materialize with every external dependency (the
// HTTP client and the cache directory) injected for testing.
func materialize(ctx context.Context, release *eksd.Release, arch string, client *http.Client, cacheDir string) (string, error) {
	asset, ok := release.ServerTarball[arch]
	if !ok {
		return "", fmt.Errorf("eks version %q has no server tarball for arch %q", release.EKSVersion, arch)
	}

	tarballPath, err := eksdcache.DownloadServerTarball(ctx, client, asset, cacheDir)
	if err != nil {
		return "", fmt.Errorf("downloading server tarball: %w", err)
	}

	componentDir := filepath.Join(cacheDir, asset.SHA256)
	if hasAllBinaries(componentDir) {
		return componentDir, nil
	}

	if err := os.RemoveAll(componentDir); err != nil {
		return "", fmt.Errorf("removing stale component dir %s: %w", componentDir, err)
	}

	if err := extractComponentDir(tarballPath, componentDir); err != nil {
		return "", err
	}

	return componentDir, nil
}

// hasAllBinaries reports whether dir already contains every entry of
// binaries as a regular file.
func hasAllBinaries(dir string) bool {
	for _, name := range binaries {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}

	return true
}
