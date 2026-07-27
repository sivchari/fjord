package kind

import (
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
)

// agentNodePort is the NodePort fjord-agent's fake STS API is published on
// inside the cluster (see internal/cluster.EnsureAgent's NodePort Service).
const agentNodePort = 30080

// Config is the subset of kind cluster configuration fjord exposes.
type Config struct {
	// Name is the cluster name.
	Name string
	// KubeVersion is the Kubernetes version the node image runs (e.g.
	// "v1.33.13"). It selects the kubeadm config API version for patches
	// and must match the node image.
	KubeVersion string
	// CoreDNSImageRepository overrides the CoreDNS image repository via a
	// kubeadm ClusterConfiguration patch. Leave empty to use kind's default.
	CoreDNSImageRepository string
	// CoreDNSImageTag overrides the CoreDNS image tag via a kubeadm
	// ClusterConfiguration patch. Leave empty to use kind's default.
	CoreDNSImageTag string
	// HostPort, if nonzero, publishes fjord-agent's NodePort Service
	// (agentNodePort) on this host port, so callers outside the cluster can
	// reach its fake STS API. Zero adds no port mapping.
	HostPort int32
}

// ToV1Alpha4 builds the kind v1alpha4.Cluster equivalent of c. v0 only
// supports a single control-plane node, so Nodes is left unset unless a
// port mapping is required, and kind applies its own single-node default.
func (c *Config) ToV1Alpha4() *v1alpha4.Cluster {
	cluster := &v1alpha4.Cluster{
		Name: c.Name,
	}

	if c.CoreDNSImageRepository != "" || c.CoreDNSImageTag != "" {
		cluster.KubeadmConfigPatches = append(cluster.KubeadmConfigPatches, coreDNSKubeadmPatch(c.KubeVersion, c.CoreDNSImageRepository, c.CoreDNSImageTag))
	}

	if c.HostPort != 0 {
		cluster.Nodes = []v1alpha4.Node{
			{
				Role: v1alpha4.ControlPlaneRole,
				ExtraPortMappings: []v1alpha4.PortMapping{
					{
						ContainerPort: agentNodePort,
						HostPort:      c.HostPort,
					},
				},
			},
		}
	}

	return cluster
}

// coreDNSKubeadmPatch builds a kubeadm ClusterConfiguration merge patch that
// overrides the CoreDNS image repository and tag. The patch apiVersion must
// match the version kind renders its kubeadm config with, or kind silently
// drops the patch.
func coreDNSKubeadmPatch(kubeVersion, repository, tag string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: ClusterConfiguration
dns:
  imageRepository: %s
  imageTag: %s
`, kubeadmAPIVersion(kubeVersion), repository, tag)
}

// kubeadmAPIVersion returns the kubeadm config API version kind renders for
// the given Kubernetes version. This mirrors kind v0.32.0's template
// selection (pkg/cluster/internal/kubeadm/config.go): v1beta4 from Kubernetes
// v1.36.0, v1beta3 below. Re-verify this cutoff when bumping kind.
func kubeadmAPIVersion(kubeVersion string) string {
	if kubeMinor(kubeVersion) >= 36 {
		return "kubeadm.k8s.io/v1beta4"
	}

	return "kubeadm.k8s.io/v1beta3"
}

// kubeMinor extracts the minor version from a "v<major>.<minor>.<patch>"
// version string. Unparsable versions return 0, selecting the older kubeadm
// API version, which every EKS-D release fjord supports understands.
func kubeMinor(kubeVersion string) int {
	parts := strings.Split(strings.TrimPrefix(kubeVersion, "v"), ".")
	if len(parts) < 2 {
		return 0
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}

	return minor
}
