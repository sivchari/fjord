package componentdir

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry describes a single entry to write into a fixture tarball.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     []byte
}

// buildServerTarball returns the gzipped tar bytes of a fixture EKS-D
// kubernetes-server tarball containing entries.
func buildServerTarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		header := &tar.Header{Name: e.name, Typeflag: typeflag, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.name, err)
		}

		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("Write(%s): %v", e.name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}

	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}

	return buf.Bytes()
}

// validEntries is a fixture EKS-D server tarball's entries: the wanted
// binaries plus a couple of entries extractComponentDir must ignore
// (kubeadm, an unrelated top-level file, and a component image tar).
func validEntries() []tarEntry {
	entries := make([]tarEntry, 0, len(binaries)+3)

	for _, name := range binaries {
		entries = append(entries, tarEntry{
			name: "kubernetes/server/bin/" + name,
			mode: 0o755,
			body: []byte("fake-" + name + "-binary"),
		})
	}

	entries = append(entries,
		tarEntry{name: "kubernetes/version", mode: 0o644, body: []byte("v1.33.13")},
		tarEntry{name: "kubernetes/server/bin/kubeadm", mode: 0o755, body: []byte("fake-kubeadm-binary")},
		tarEntry{name: "kubernetes/server/bin/kube-apiserver.tar", mode: 0o644, body: []byte("fake-image-tar")},
	)

	return entries
}

// writeTarballFile writes content to a new file under t.TempDir() and
// returns its path.
func writeTarballFile(t *testing.T, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubernetes-server-linux-amd64.tar.gz")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return path
}

func TestExtractComponentDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []tarEntry
		wantErr string
	}{
		{
			name:    "all binaries present",
			entries: validEntries(),
		},
		{
			name: "missing a binary",
			entries: []tarEntry{
				{name: "kubernetes/server/bin/kube-apiserver", mode: 0o755, body: []byte("fake")},
			},
			wantErr: "missing binaries",
		},
		{
			name: "path traversal entry",
			entries: append(validEntries(), tarEntry{
				name: "../../../../tmp/fjord-componentdir-escape-test",
				mode: 0o644,
				body: []byte("pwned"),
			}),
			wantErr: "escapes destination directory",
		},
		{
			name: "non-regular entry masquerading as a wanted binary",
			entries: append(
				[]tarEntry{{name: "kubernetes/server/bin/kube-apiserver", typeflag: tar.TypeSymlink, mode: 0o755}},
				validEntries()[1:]...,
			),
			wantErr: "expected a regular file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tarballPath := writeTarballFile(t, buildServerTarball(t, tt.entries))
			finalDir := filepath.Join(t.TempDir(), "component-dir")

			err := extractComponentDir(tarballPath, finalDir)

			if tt.wantErr == "" {
				assertExtractedSuccessfully(t, finalDir, err)

				return
			}

			assertExtractionFailed(t, finalDir, err, tt.wantErr)
		})
	}
}

// assertExtractedSuccessfully asserts that extractComponentDir succeeded
// and that finalDir contains every wanted binary, executable, with its
// original content.
func assertExtractedSuccessfully(t *testing.T, finalDir string, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("extractComponentDir: %v", err)
	}

	for _, name := range binaries {
		path := filepath.Join(finalDir, name)

		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", name, statErr)
		}

		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s: mode = %v, want executable bit set", name, info.Mode())
		}

		got, readErr := os.ReadFile(filepath.Clean(path))
		if readErr != nil {
			t.Fatalf("ReadFile(%s): %v", name, readErr)
		}

		if want := "fake-" + name + "-binary"; string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
}

// assertExtractionFailed asserts that extractComponentDir failed with an
// error containing wantErr and left no trace of finalDir.
func assertExtractionFailed(t *testing.T, finalDir string, err error, wantErr string) {
	t.Helper()

	if err == nil {
		t.Fatalf("extractComponentDir: want error containing %q, got nil", wantErr)
	}

	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("extractComponentDir error = %q, want it to contain %q", err.Error(), wantErr)
	}

	if _, statErr := os.Stat(finalDir); !os.IsNotExist(statErr) {
		t.Errorf("finalDir should not exist after a failed extraction, stat err = %v", statErr)
	}
}
