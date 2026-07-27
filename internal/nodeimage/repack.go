package nodeimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sivchari/fjord/internal/imagetar"
)

// serverBinComponents are the kubernetes/server/bin/<name>.tar image
// archives whose embedded EKS-D image references get rewritten to their
// upstream registry.k8s.io equivalents.
var serverBinComponents = []string{
	"kube-apiserver",
	"kube-controller-manager",
	"kube-scheduler",
	"kube-proxy",
}

// eksDistroKubernetesRepository is the EKS-D container image repository
// prefix (excluding component name) rewritten to its upstream
// registry.k8s.io equivalent.
const (
	eksDistroKubernetesRepository = "public.ecr.aws/eks-distro/kubernetes"
	upstreamKubernetesRepository  = "registry.k8s.io"
)

// RewriteServerTarball extracts the EKS-D kubernetes-server tarball at
// srcPath, rewrites the EKS-D image tags embedded in each
// kubernetes/server/bin/<component>.tar to their upstream registry.k8s.io
// equivalents, and repacks the result as a new tar.gz file (preserving the
// top-level "kubernetes/" directory structure kind's node image builder
// expects). It returns the path to the rewritten tarball and a cleanup
// function that removes every temporary file RewriteServerTarball created;
// cleanup is safe to call even after an error and is never nil.
func RewriteServerTarball(srcPath string) (string, func(), error) {
	workDir, err := os.MkdirTemp("", "fjord-nodeimage-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating work dir: %w", err)
	}

	cleanup := func() { _ = os.RemoveAll(workDir) }

	rewrittenPath, err := rewriteServerTarballInto(srcPath, workDir)
	if err != nil {
		cleanup()

		return "", func() {}, err
	}

	return rewrittenPath, cleanup, nil
}

// rewriteServerTarballInto does the extract/rewrite/repack work for
// RewriteServerTarball, using workDir for all intermediate files.
func rewriteServerTarballInto(srcPath, workDir string) (string, error) {
	extractDir := filepath.Join(workDir, "extracted")
	if err := extractTarGz(srcPath, extractDir); err != nil {
		return "", err
	}

	binDir := filepath.Join(extractDir, "kubernetes", "server", "bin")
	if err := rewriteServerBinTars(binDir); err != nil {
		return "", err
	}

	destPath := filepath.Join(workDir, "rewritten.tar.gz")
	if err := packTarGz(extractDir, destPath); err != nil {
		return "", err
	}

	return destPath, nil
}

// rewriteServerBinTars rewrites the EKS-D image tags embedded in each
// serverBinComponents tar under binDir.
func rewriteServerBinTars(binDir string) error {
	for _, component := range serverBinComponents {
		path := filepath.Join(binDir, component+".tar")
		if err := rewriteComponentTar(path, component); err != nil {
			return err
		}
	}

	return nil
}

// rewriteComponentTar rewrites the EKS-D image references for component
// embedded in the docker-save-style tar at path, in place.
func rewriteComponentTar(path, component string) error {
	path = filepath.Clean(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	replacements := map[string]string{
		eksDistroKubernetesRepository + "/" + component: upstreamKubernetesRepository + "/" + component,
	}

	var buf bytes.Buffer

	if err := imagetar.RewriteTags(&buf, bytes.NewReader(data), replacements); err != nil {
		return fmt.Errorf("rewriting %s: %w", path, err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// extractTarGz decompresses and unpacks the tar.gz file at srcPath into
// destDir, preserving directory structure, file modes, and symlinks.
func extractTarGz(srcPath, destDir string) error {
	srcPath = filepath.Clean(srcPath)

	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", srcPath, err)
	}

	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("opening gzip reader for %s: %w", srcPath, err)
	}

	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("reading tar header from %s: %w", srcPath, err)
		}

		if err := extractEntry(destDir, header, tr); err != nil {
			return err
		}
	}

	return nil
}

// extractEntry extracts a single tar entry into destDir.
func extractEntry(destDir string, header *tar.Header, tr *tar.Reader) error {
	target, err := safeJoin(destDir, header.Name)
	if err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o750); err != nil {
			return fmt.Errorf("creating directory %s: %w", target, err)
		}
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("creating directory for %s: %w", target, err)
		}

		if err := os.Symlink(header.Linkname, target); err != nil {
			return fmt.Errorf("creating symlink %s: %w", target, err)
		}
	default:
		if err := extractFile(target, header, tr); err != nil {
			return err
		}
	}

	return nil
}

// safeJoin joins destDir and name, rejecting names that would escape
// destDir (a zip-slip guard for tar entries with ".." components).
func safeJoin(destDir, name string) (string, error) {
	target := filepath.Join(destDir, name)
	cleanDestDir := filepath.Clean(destDir)

	if target != cleanDestDir && !strings.HasPrefix(target, cleanDestDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar entry %q escapes destination directory", name)
	}

	return target, nil
}

// extractFile writes a single regular-file tar entry to target, preserving
// its mode.
func extractFile(target string, header *tar.Header, tr *tar.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating directory for %s: %w", target, err)
	}

	target = filepath.Clean(target)

	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, header.FileInfo().Mode())
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}

	defer func() { _ = out.Close() }()

	if _, err := io.CopyN(out, tr, header.Size); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("writing %s: %w", target, err)
	}

	return nil
}

// packTarGz walks srcDir and writes its contents (paths relative to
// srcDir, so srcDir's own immediate children become the archive's
// top-level entries) as a tar.gz file at destPath.
func packTarGz(srcDir, destPath string) error {
	out, err := os.Create(filepath.Clean(destPath))
	if err != nil {
		return fmt.Errorf("creating %s: %w", destPath, err)
	}

	defer func() { _ = out.Close() }()

	gzw := gzip.NewWriter(out)
	tw := tar.NewWriter(gzw)

	walkErr := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if path == srcDir {
			return nil
		}

		return addTarEntry(tw, srcDir, path, d)
	})
	if walkErr != nil {
		return fmt.Errorf("packing %s: %w", destPath, walkErr)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar writer for %s: %w", destPath, err)
	}

	if err := gzw.Close(); err != nil {
		return fmt.Errorf("closing gzip writer for %s: %w", destPath, err)
	}

	return nil
}

// addTarEntry writes the tar header (and, for regular files, content) for
// the file system entry at path into tw, naming it path relative to root.
func addTarEntry(tw *tar.Writer, root, path string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("relativizing %s: %w", path, err)
	}

	link := ""
	if info.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			return fmt.Errorf("reading symlink %s: %w", path, err)
		}
	}

	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("building tar header for %s: %w", path, err)
	}

	header.Name = filepath.ToSlash(rel)

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", rel, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	return writeTarFileContent(tw, path, rel)
}

// writeTarFileContent copies the content of the regular file at path
// (named rel in the archive, for error messages) into tw.
func writeTarFileContent(tw *tar.Writer, path, rel string) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}

	return nil
}
