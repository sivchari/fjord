package eksdcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sivchari/fjord/internal/eksd"
)

// DefaultCacheDir returns fjord's default download cache directory for
// EKS-D server tarballs, ~/.cache/fjord/eksd (or the OS equivalent of
// os.UserCacheDir).
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving user cache dir: %w", err)
	}

	return filepath.Join(base, "fjord", "eksd"), nil
}

// DownloadServerTarball ensures asset is present under cacheDir, keyed by
// its SHA256 checksum, downloading it via client only if the cache is
// missing or its content does not match asset.SHA256. It returns the path
// to the verified, cached tarball.
func DownloadServerTarball(ctx context.Context, client *http.Client, asset eksd.Asset, cacheDir string) (string, error) {
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return "", fmt.Errorf("creating cache dir %s: %w", cacheDir, err)
	}

	path := filepath.Join(cacheDir, asset.SHA256+".tar.gz")

	if fileMatchesSHA256(path, asset.SHA256) {
		return path, nil
	}

	if err := downloadFile(ctx, client, asset.URI, path); err != nil {
		return "", err
	}

	if !fileMatchesSHA256(path, asset.SHA256) {
		_ = os.Remove(path)

		return "", fmt.Errorf("downloaded tarball %s: sha256 mismatch, want %s", asset.URI, asset.SHA256)
	}

	return path, nil
}

// fileMatchesSHA256 reports whether the file at path exists and its
// content hashes to want.
func fileMatchesSHA256(path, want string) bool {
	path = filepath.Clean(path)

	f, err := os.Open(path)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}

	return hex.EncodeToString(h.Sum(nil)) == want
}

// downloadFile streams url into dest via a temporary file in the same
// directory, renamed into place on success so a failed download never
// leaves a partial file at dest.
func downloadFile(ctx context.Context, client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("building request for %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d downloading %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", dest, err)
	}

	return writeAndRename(tmp, resp.Body, dest)
}

// writeAndRename copies src into tmp, then atomically renames tmp to dest.
// tmp is removed on any failure.
func writeAndRename(tmp *os.File, src io.Reader, dest string) error {
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("writing %s: %w", dest, err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("closing %s: %w", dest, err)
	}

	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("renaming %s to %s: %w", tmpPath, dest, err)
	}

	return nil
}
