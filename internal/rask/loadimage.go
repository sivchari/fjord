package rask

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	raskcluster "github.com/sivchari/rask/pkg/cluster"
)

// LoadDockerImage implements clusterprovider.Provider. It saves the local
// docker image imageRef and streams it directly into the cluster named
// name's image store, equivalent to `rask load docker-image` (see
// cmd/rask/load.go in the rask module, whose loadOneDockerImage this
// mirrors): docker save's stdout is piped straight into
// raskcluster.Provider.LoadImages without ever buffering the whole archive
// to disk or memory first. imageRef must already exist in the local docker
// image store (e.g. built via `docker build`).
func (p *provider) LoadDockerImage(ctx context.Context, name, imageRef string) error {
	save := exec.CommandContext(ctx, "docker", "save", imageRef)

	stdout, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piping docker save output: %w", err)
	}

	var stderr bytes.Buffer

	save.Stderr = &stderr

	if err := save.Start(); err != nil {
		return fmt.Errorf("starting docker save (is Docker installed and the daemon running?): %w", err)
	}

	loadErr := p.inner.LoadImages(ctx, name, []raskcluster.ImageSource{{Reference: imageRef, Stream: stdout}})
	waitErr := save.Wait()

	if waitErr != nil {
		return fmt.Errorf("docker save %q: %w (%s)", imageRef, waitErr, strings.TrimSpace(stderr.String()))
	}

	if loadErr != nil {
		return fmt.Errorf("load image %q onto cluster %q: %w", imageRef, name, loadErr)
	}

	return nil
}
