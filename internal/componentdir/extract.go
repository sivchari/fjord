package componentdir

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// binaries are the component binaries rask's --component-dir requires,
// found under kubernetes/server/bin/ in the EKS-D kubernetes-server
// tarball.
var binaries = []string{
	"kube-apiserver",
	"kube-controller-manager",
	"kube-scheduler",
	"kubelet",
	"kube-proxy",
}

// serverBinDir is the directory binaries live under inside the EKS-D
// kubernetes-server tarball.
const serverBinDir = "kubernetes/server/bin/"

// extractComponentDir extracts binaries from the EKS-D kubernetes-server
// tarball at tarballPath into finalDir, which must not already exist.
// Extraction happens into a temporary sibling directory first and is
// renamed into place only once every binary has been found, so finalDir
// never contains a partial extraction.
func extractComponentDir(tarballPath, finalDir string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(finalDir), ".componentdir-*")
	if err != nil {
		return fmt.Errorf("creating temp extraction dir: %w", err)
	}

	if err := extractBinaries(tarballPath, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)

		return err
	}

	if err := os.Rename(tmpDir, finalDir); err != nil {
		_ = os.RemoveAll(tmpDir)

		return fmt.Errorf("renaming %s to %s: %w", tmpDir, finalDir, err)
	}

	return nil
}

// extractBinaries reads the gzipped tar at tarballPath and writes every
// entry under serverBinDir whose name matches one of binaries into destDir,
// flattened (destDir/<name>, not destDir/kubernetes/server/bin/<name>) and
// preserving the tar entry's file mode. Every other entry is skipped, but
// its name is still checked for path traversal. extractBinaries returns an
// error unless every entry in binaries was found.
func extractBinaries(tarballPath, destDir string) error {
	tarballPath = filepath.Clean(tarballPath)

	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", tarballPath, err)
	}

	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("opening gzip reader for %s: %w", tarballPath, err)
	}

	defer func() { _ = gzr.Close() }()

	want := make(map[string]bool, len(binaries))
	for _, name := range binaries {
		want[name] = true
	}

	found := make(map[string]bool, len(binaries))
	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("reading tar header from %s: %w", tarballPath, err)
		}

		// Reject any entry whose name would escape destDir, regardless of
		// whether it is one of the binaries this extraction needs.
		if _, err := safeJoin(destDir, header.Name); err != nil {
			return err
		}

		name, ok := strings.CutPrefix(header.Name, serverBinDir)
		if !ok || !want[name] {
			continue
		}

		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s: expected a regular file, got type %d", header.Name, header.Typeflag)
		}

		if err := extractBinary(tr, header, filepath.Join(destDir, name)); err != nil {
			return err
		}

		found[name] = true
	}

	return requireAllFound(tarballPath, found)
}

// requireAllFound returns an error naming every entry of binaries absent
// from found.
func requireAllFound(tarballPath string, found map[string]bool) error {
	if len(found) == len(binaries) {
		return nil
	}

	missing := make([]string, 0, len(binaries)-len(found))

	for _, name := range binaries {
		if !found[name] {
			missing = append(missing, name)
		}
	}

	return fmt.Errorf("%s: missing binaries in kubernetes/server/bin: %s", tarballPath, strings.Join(missing, ", "))
}

// extractBinary writes tr's remaining content (header's entry) to target,
// preserving header's file mode (including the executable bit).
func extractBinary(tr *tar.Reader, header *tar.Header, target string) error {
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
