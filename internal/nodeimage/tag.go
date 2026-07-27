package nodeimage

import (
	"fmt"

	"github.com/sivchari/fjord/internal/eksd"
)

// nodeImageRepository is the GHCR repository fjord publishes node images
// under.
const nodeImageRepository = "ghcr.io/sivchari/fjord/node"

// Tag returns the pinned node image tag for release, e.g.
// "ghcr.io/sivchari/fjord/node:v1.33.13-eks-1-33-29".
func Tag(release *eksd.Release) string {
	return fmt.Sprintf("%s:%s-eks-%s-%d", nodeImageRepository, release.KubeVersion, release.Channel, release.Number)
}

// FloatingTag returns the floating node image tag for release's EKS
// Kubernetes minor version, e.g. "ghcr.io/sivchari/fjord/node:1.33". This
// tag is repointed to the latest EKS-D patch as new releases are published.
func FloatingTag(release *eksd.Release) string {
	return fmt.Sprintf("%s:%s", nodeImageRepository, release.EKSVersion)
}
