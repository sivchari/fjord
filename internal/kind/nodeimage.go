package kind

import (
	"fmt"

	"sigs.k8s.io/kind/pkg/build/nodeimage"
	"sigs.k8s.io/kind/pkg/log"
)

// BuildNodeImage builds a kind node image from the Kubernetes build tarball
// at tarballPath, tagging the resulting image imageTag for architecture arch.
func BuildNodeImage(tarballPath, imageTag, arch string, logger log.Logger) error {
	if err := nodeimage.Build(
		nodeimage.WithBuildType("file"),
		nodeimage.WithKubeParam(tarballPath),
		nodeimage.WithImage(imageTag),
		nodeimage.WithArch(arch),
		nodeimage.WithLogger(logger),
	); err != nil {
		return fmt.Errorf("build node image %q from %q: %w", imageTag, tarballPath, err)
	}

	return nil
}
