package componentdir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/fjord/internal/eksd"
)

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestMaterialize_Success(t *testing.T) {
	t.Parallel()

	content := buildServerTarball(t, validEntries())
	sum := sha256Hex(t, content)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	release := &eksd.Release{
		EKSVersion:    "1.33",
		ServerTarball: map[string]eksd.Asset{"amd64": {URI: server.URL, SHA256: sum}},
	}

	cacheDir := t.TempDir()

	dir, err := materialize(context.Background(), release, "amd64", server.Client(), cacheDir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if want := filepath.Join(cacheDir, sum); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}

	for _, name := range binaries {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
			t.Errorf("stat %s: %v", name, statErr)
		}
	}

	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}

	// A second call must reuse both the downloaded tarball and the
	// extracted component directory, so it makes no further requests.
	dir2, err := materialize(context.Background(), release, "amd64", server.Client(), cacheDir)
	if err != nil {
		t.Fatalf("materialize (second call): %v", err)
	}

	if dir2 != dir {
		t.Errorf("dir2 = %q, want %q", dir2, dir)
	}

	if requests != 1 {
		t.Errorf("requests after second call = %d, want 1 (should reuse cache)", requests)
	}
}

func TestMaterialize_UnsupportedArch(t *testing.T) {
	t.Parallel()

	release := &eksd.Release{
		EKSVersion:    "1.33",
		ServerTarball: map[string]eksd.Asset{"amd64": {URI: "http://example.invalid", SHA256: "deadbeef"}},
	}

	if _, err := materialize(context.Background(), release, "arm64", http.DefaultClient, t.TempDir()); err == nil {
		t.Fatal("materialize: want error for unsupported arch, got nil")
	}
}

func TestMaterialize_ReextractsWhenComponentDirIsIncomplete(t *testing.T) {
	t.Parallel()

	content := buildServerTarball(t, validEntries())
	sum := sha256Hex(t, content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	release := &eksd.Release{
		EKSVersion:    "1.33",
		ServerTarball: map[string]eksd.Asset{"amd64": {URI: server.URL, SHA256: sum}},
	}

	cacheDir := t.TempDir()

	// Simulate a partial extraction left behind by a previous crashed run:
	// the component directory exists but is missing binaries.
	staleDir := filepath.Join(cacheDir, sum)
	if err := os.MkdirAll(staleDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := os.WriteFile(filepath.Join(staleDir, "kube-apiserver"), []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dir, err := materialize(context.Background(), release, "amd64", server.Client(), cacheDir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	for _, name := range binaries {
		got, readErr := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
		if readErr != nil {
			t.Fatalf("ReadFile(%s): %v", name, readErr)
		}

		if want := "fake-" + name + "-binary"; string(got) != want {
			t.Errorf("%s content = %q, want %q (stale extraction should have been replaced)", name, got, want)
		}
	}
}
