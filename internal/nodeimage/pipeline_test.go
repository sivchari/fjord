package nodeimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/logger"
)

// fakeBuildCall records a single invocation of a buildNodeImageFunc.
type fakeBuildCall struct {
	tarballPath string
	imageTag    string
	arch        string
}

// fixtureServerTarball builds the bytes of a minimal, valid EKS-D
// kubernetes-server tarball: a top-level "kubernetes/" directory with a
// version file and four empty (but validly-formed) component image tars,
// enough for RewriteServerTarball to round-trip successfully.
func fixtureServerTarball(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	writeEntry := func(name string, mode int64, body []byte) {
		header := &tar.Header{Name: name, Mode: mode, Size: int64(len(body))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}

		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}

	writeEntry("kubernetes/version", 0o644, []byte("v1.33.13"))

	for _, component := range serverBinComponents {
		writeEntry("kubernetes/server/bin/"+component+".tar", 0o644, emptyTar(t))
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}

	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}

	return buf.Bytes()
}

// emptyTar returns the bytes of a valid, empty tar archive.
func emptyTar(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	if err := tar.NewWriter(&buf).Close(); err != nil {
		t.Fatalf("empty tar Close: %v", err)
	}

	return buf.Bytes()
}

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestBuild_Success(t *testing.T) {
	t.Parallel()

	content := fixtureServerTarball(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	release := &eksd.Release{
		EKSVersion:  "1.33",
		Channel:     "1-33",
		Number:      29,
		KubeVersion: "v1.33.13",
		ServerTarball: map[string]eksd.Asset{
			"amd64": {URI: server.URL, SHA256: sha256Hex(t, content)},
		},
	}

	var calls []fakeBuildCall

	fakeBuild := func(tarballPath, imageTag, arch string, _ logger.Logger) error {
		calls = append(calls, fakeBuildCall{tarballPath: tarballPath, imageTag: imageTag, arch: arch})

		if _, err := os.Stat(tarballPath); err != nil {
			t.Errorf("tarball %s does not exist: %v", tarballPath, err)
		}

		return nil
	}

	imageTag, err := build(context.Background(), release, "amd64", logger.NewStderr(io.Discard, 0), server.Client(), t.TempDir(), fakeBuild)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := Tag(release)
	if imageTag != want {
		t.Errorf("imageTag = %q, want %q", imageTag, want)
	}

	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}

	if calls[0].imageTag != want {
		t.Errorf("calls[0].imageTag = %q, want %q", calls[0].imageTag, want)
	}

	if calls[0].arch != "amd64" {
		t.Errorf("calls[0].arch = %q, want %q", calls[0].arch, "amd64")
	}
}

func TestBuild_UnsupportedArch(t *testing.T) {
	t.Parallel()

	release := &eksd.Release{
		EKSVersion:    "1.33",
		Channel:       "1-33",
		Number:        29,
		KubeVersion:   "v1.33.13",
		ServerTarball: map[string]eksd.Asset{"amd64": {URI: "http://example.invalid", SHA256: "deadbeef"}},
	}

	fakeBuild := func(string, string, string, logger.Logger) error {
		t.Fatal("buildNodeImage should not be called when arch has no server tarball")

		return nil
	}

	if _, err := build(context.Background(), release, "arm64", logger.NewStderr(io.Discard, 0), http.DefaultClient, t.TempDir(), fakeBuild); err == nil {
		t.Fatal("build: want error for unsupported arch, got nil")
	}
}

func TestBuild_PropagatesBuildNodeImageError(t *testing.T) {
	t.Parallel()

	content := fixtureServerTarball(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	release := &eksd.Release{
		EKSVersion:  "1.33",
		Channel:     "1-33",
		Number:      29,
		KubeVersion: "v1.33.13",
		ServerTarball: map[string]eksd.Asset{
			"amd64": {URI: server.URL, SHA256: sha256Hex(t, content)},
		},
	}

	fakeBuild := func(string, string, string, logger.Logger) error {
		return os.ErrPermission
	}

	if _, err := build(context.Background(), release, "amd64", logger.NewStderr(io.Discard, 0), server.Client(), t.TempDir(), fakeBuild); err == nil {
		t.Fatal("build: want error, got nil")
	}
}
