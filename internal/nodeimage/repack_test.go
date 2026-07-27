package nodeimage_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/fjord/internal/nodeimage"
)

// fixtureFile describes a single file to embed in a fixture EKS-D server
// tarball.
type fixtureFile struct {
	name string
	mode int64
	body []byte
}

// componentTar builds a docker-save-style tar containing a manifest.json
// that references the given EKS-D image for component.
func componentTar(t *testing.T, component string) []byte {
	t.Helper()

	manifest := `[{"Config":"blobs/sha256/aaaa","RepoTags":["public.ecr.aws/eks-distro/kubernetes/` + component + `:v1.33.13-eks-1-33-29"],"Layers":["blobs/sha256/bbbb"]}]`

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	writeTarFile(t, tw, "manifest.json", 0o644, []byte(manifest))
	writeTarFile(t, tw, "blobs/sha256/aaaa", 0o644, []byte("config-blob"))

	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}

	return buf.Bytes()
}

func writeTarFile(t *testing.T, tw *tar.Writer, name string, mode int64, body []byte) {
	t.Helper()

	header := &tar.Header{
		Name: name,
		Mode: mode,
		Size: int64(len(body)),
	}

	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("WriteHeader(%s): %v", name, err)
	}

	if _, err := tw.Write(body); err != nil {
		t.Fatalf("Write(%s): %v", name, err)
	}
}

// buildFixtureServerTarball builds a minimal EKS-D kubernetes-server
// tarball: a top-level "kubernetes/" directory containing a version file,
// the three CLI binaries, and the four component image tars.
func buildFixtureServerTarball(t *testing.T) string {
	t.Helper()

	components := []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy"}

	files := make([]fixtureFile, 0, 4+len(components))
	files = append(files,
		fixtureFile{name: "kubernetes/version", mode: 0o644, body: []byte("v1.33.13")},
		fixtureFile{name: "kubernetes/server/bin/kubeadm", mode: 0o755, body: []byte("fake-kubeadm-binary")},
		fixtureFile{name: "kubernetes/server/bin/kubelet", mode: 0o755, body: []byte("fake-kubelet-binary")},
		fixtureFile{name: "kubernetes/server/bin/kubectl", mode: 0o755, body: []byte("fake-kubectl-binary")},
	)

	for _, c := range components {
		files = append(files, fixtureFile{
			name: "kubernetes/server/bin/" + c + ".tar",
			mode: 0o644,
			body: componentTar(t, c),
		})
	}

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "kubernetes-server-linux-amd64.tar.gz")

	f, err := os.Create(filepath.Clean(srcPath))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	defer func() { _ = f.Close() }()

	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)

	for _, file := range files {
		writeTarFile(t, tw, file.name, file.mode, file.body)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}

	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}

	return srcPath
}

// openTarGz opens the tar.gz file at path and returns a *tar.Reader over
// it, plus a cleanup func that closes the underlying gzip and file
// handles.
func openTarGz(t *testing.T, path string) (*tar.Reader, func()) {
	t.Helper()

	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	gzr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}

	cleanup := func() {
		_ = gzr.Close()
		_ = f.Close()
	}

	return tar.NewReader(gzr), cleanup
}

// readTarGzMembers reads every regular-file member of a tar.gz file into
// memory, keyed by name.
func readTarGzMembers(t *testing.T, path string) map[string][]byte {
	t.Helper()

	tr, cleanup := openTarGz(t, path)
	defer cleanup()

	members := make(map[string][]byte)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", header.Name, err)
		}

		members[header.Name] = body
	}

	return members
}

func TestRewriteServerTarball_RewritesComponentImageTags(t *testing.T) {
	t.Parallel()

	src := buildFixtureServerTarball(t)

	rewrittenPath, cleanup, err := nodeimage.RewriteServerTarball(src)
	if err != nil {
		t.Fatalf("RewriteServerTarball: %v", err)
	}

	t.Cleanup(cleanup)

	members := readTarGzMembers(t, rewrittenPath)

	for _, component := range []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy"} {
		componentTarPath := "kubernetes/server/bin/" + component + ".tar"

		componentTarBody, ok := members[componentTarPath]
		if !ok {
			t.Fatalf("missing member %s", componentTarPath)
		}

		manifest := extractManifestJSON(t, componentTarBody)

		if bytes.Contains(manifest, []byte("public.ecr.aws")) {
			t.Errorf("%s manifest.json still references public.ecr.aws: %s", componentTarPath, manifest)
		}

		want := []byte(`"registry.k8s.io/` + component + `:v1.33.13-eks-1-33-29"`)
		if !bytes.Contains(manifest, want) {
			t.Errorf("%s manifest.json missing rewritten tag %s: %s", componentTarPath, want, manifest)
		}
	}
}

func TestRewriteServerTarball_PreservesNonImageFilesAndPermissions(t *testing.T) {
	t.Parallel()

	src := buildFixtureServerTarball(t)

	rewrittenPath, cleanup, err := nodeimage.RewriteServerTarball(src)
	if err != nil {
		t.Fatalf("RewriteServerTarball: %v", err)
	}

	t.Cleanup(cleanup)

	sawVersion, kubeadmMode := scanVersionAndKubeadmMode(t, rewrittenPath)

	if !sawVersion {
		t.Error("rewritten tarball is missing kubernetes/version")
	}

	if kubeadmMode&0o111 == 0 {
		t.Errorf("kubeadm mode = %o, want executable bits preserved", kubeadmMode)
	}
}

// scanVersionAndKubeadmMode reads the rewritten tarball at path, checking
// that kubernetes/version still contains the expected content and
// returning the on-disk mode of kubernetes/server/bin/kubeadm.
func scanVersionAndKubeadmMode(t *testing.T, path string) (bool, int64) {
	t.Helper()

	tr, cleanup := openTarGz(t, path)
	defer cleanup()

	var (
		sawVersion  bool
		kubeadmMode int64
	)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}

		switch header.Name {
		case "kubernetes/version":
			sawVersion = true

			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("ReadAll(kubernetes/version): %v", err)
			}

			if string(body) != "v1.33.13" {
				t.Errorf("kubernetes/version = %q, want %q", body, "v1.33.13")
			}
		case "kubernetes/server/bin/kubeadm":
			kubeadmMode = header.Mode
		}
	}

	return sawVersion, kubeadmMode
}

func extractManifestJSON(t *testing.T, tarBody []byte) []byte {
	t.Helper()

	tr := tar.NewReader(bytes.NewReader(tarBody))

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("tar Next: %v", err)
		}

		if header.Name != "manifest.json" {
			continue
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll(manifest.json): %v", err)
		}

		return body
	}

	t.Fatal("manifest.json not found")

	return nil
}
