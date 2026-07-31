package kind

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

// LoadDockerImage implements clusterprovider.Provider. It saves the local
// docker image imageRef and loads it onto every node of the cluster named
// name, equivalent to `kind load docker-image`. imageRef must already exist
// in the local docker image store (e.g. built via `docker build`).
func (p *provider) LoadDockerImage(ctx context.Context, name, imageRef string) error {
	nodeList, err := p.inner.ListInternalNodes(name)
	if err != nil {
		return fmt.Errorf("list nodes for cluster %q: %w", name, err)
	}

	if len(nodeList) == 0 {
		return fmt.Errorf("no nodes found for cluster %q", name)
	}

	tarPath, cleanup, err := saveDockerImage(ctx, imageRef)
	if err != nil {
		return err
	}

	defer cleanup()

	for _, node := range nodeList {
		if err := loadImageArchive(node, tarPath); err != nil {
			return fmt.Errorf("load image %q onto node %q: %w", imageRef, node.String(), err)
		}
	}

	return nil
}

// saveDockerImage runs `docker save` to export imageRef into a temporary
// tarball, returning its path and a cleanup function that removes it.
func saveDockerImage(ctx context.Context, imageRef string) (tarPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "fjord-load-image")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}

	cleanup = func() { _ = os.RemoveAll(dir) }
	tarPath = filepath.Join(dir, "image.tar")

	if out, err := exec.CommandContext(ctx, "docker", "save", "-o", tarPath, imageRef).CombinedOutput(); err != nil {
		cleanup()

		return "", nil, fmt.Errorf("docker save %q: %w: %s", imageRef, err, out)
	}

	return tarPath, cleanup, nil
}

// loadImageArchive loads the image tarball at tarPath onto node.
func loadImageArchive(node nodes.Node, tarPath string) error {
	f, err := os.Open(filepath.Clean(tarPath))
	if err != nil {
		return fmt.Errorf("open image archive: %w", err)
	}

	defer func() { _ = f.Close() }()

	if err := nodeutils.LoadImageArchive(node, f); err != nil {
		return fmt.Errorf("load image archive: %w", err)
	}

	return nil
}
