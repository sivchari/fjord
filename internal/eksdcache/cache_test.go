package eksdcache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/eksdcache"
)

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestDownloadServerTarball_DownloadsAndVerifies(t *testing.T) {
	t.Parallel()

	content := []byte("fake tarball content")
	sum := sha256Hex(t, content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	asset := eksd.Asset{URI: server.URL, SHA256: sum}

	path, err := eksdcache.DownloadServerTarball(context.Background(), server.Client(), asset, cacheDir)
	if err != nil {
		t.Fatalf("DownloadServerTarball: %v", err)
	}

	want := filepath.Join(cacheDir, sum+".tar.gz")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	got, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestDownloadServerTarball_SkipsWhenCacheIsFresh(t *testing.T) {
	t.Parallel()

	content := []byte("already cached content")
	sum := sha256Hex(t, content)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	cachedPath := filepath.Join(cacheDir, sum+".tar.gz")

	if err := os.WriteFile(cachedPath, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	asset := eksd.Asset{URI: server.URL, SHA256: sum}

	path, err := eksdcache.DownloadServerTarball(context.Background(), server.Client(), asset, cacheDir)
	if err != nil {
		t.Fatalf("DownloadServerTarball: %v", err)
	}

	if path != cachedPath {
		t.Errorf("path = %q, want %q", path, cachedPath)
	}

	if requests != 0 {
		t.Errorf("requests = %d, want 0 (cache should have been reused)", requests)
	}
}

func TestDownloadServerTarball_RedownloadsWhenCacheIsStale(t *testing.T) {
	t.Parallel()

	content := []byte("fresh content from server")
	sum := sha256Hex(t, content)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	cachedPath := filepath.Join(cacheDir, sum+".tar.gz")

	if err := os.WriteFile(cachedPath, []byte("stale, mismatched content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	asset := eksd.Asset{URI: server.URL, SHA256: sum}

	path, err := eksdcache.DownloadServerTarball(context.Background(), server.Client(), asset, cacheDir)
	if err != nil {
		t.Fatalf("DownloadServerTarball: %v", err)
	}

	if requests != 1 {
		t.Errorf("requests = %d, want 1 (stale cache should trigger a redownload)", requests)
	}

	got, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestDownloadServerTarball_ChecksumMismatchReturnsError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("content that does not match the expected checksum"))
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	wantSum := sha256Hex(t, []byte("something else entirely"))
	asset := eksd.Asset{URI: server.URL, SHA256: wantSum}

	_, err := eksdcache.DownloadServerTarball(context.Background(), server.Client(), asset, cacheDir)
	if err == nil {
		t.Fatal("DownloadServerTarball: want error on checksum mismatch, got nil")
	}

	if _, statErr := os.Stat(filepath.Join(cacheDir, wantSum+".tar.gz")); !os.IsNotExist(statErr) {
		t.Errorf("cached file should have been removed after checksum mismatch, stat err = %v", statErr)
	}
}

func TestDownloadServerTarball_HTTPErrorStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	asset := eksd.Asset{URI: server.URL, SHA256: sha256Hex(t, []byte("anything"))}

	if _, err := eksdcache.DownloadServerTarball(context.Background(), server.Client(), asset, cacheDir); err == nil {
		t.Fatal("DownloadServerTarball: want error on HTTP 404, got nil")
	}
}

func TestDefaultCacheDir(t *testing.T) {
	t.Parallel()

	dir, err := eksdcache.DefaultCacheDir()
	if err != nil {
		t.Fatalf("DefaultCacheDir: %v", err)
	}

	if got, want := filepath.Base(dir), "eksd"; got != want {
		t.Errorf("base(dir) = %q, want %q", got, want)
	}

	if got, want := filepath.Base(filepath.Dir(dir)), "fjord"; got != want {
		t.Errorf("base(dir(dir)) = %q, want %q", got, want)
	}
}
