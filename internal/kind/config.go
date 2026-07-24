package kind

import (
	"fmt"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
)

// Config is the subset of kind cluster configuration fjord exposes.
type Config struct {
	// Name is the cluster name.
	Name string
	// CoreDNSImageRepository overrides the CoreDNS image repository via a
	// kubeadm ClusterConfiguration patch. Leave empty to use kind's default.
	CoreDNSImageRepository string
	// CoreDNSImageTag overrides the CoreDNS image tag via a kubeadm
	// ClusterConfiguration patch. Leave empty to use kind's default.
	CoreDNSImageTag string
}

// ToV1Alpha4 builds the kind v1alpha4.Cluster equivalent of c. v0 only
// supports a single control-plane node, so Nodes is left unset and kind
// applies its own default.
func (c *Config) ToV1Alpha4() *v1alpha4.Cluster {
	cluster := &v1alpha4.Cluster{
		Name: c.Name,
	}

	if c.CoreDNSImageRepository != "" || c.CoreDNSImageTag != "" {
		cluster.KubeadmConfigPatches = append(cluster.KubeadmConfigPatches, coreDNSKubeadmPatch(c.CoreDNSImageRepository, c.CoreDNSImageTag))
	}

	return cluster
}

// coreDNSKubeadmPatch builds a kubeadm ClusterConfiguration merge patch that
// overrides the CoreDNS image repository and tag.
func coreDNSKubeadmPatch(repository, tag string) string {
	return fmt.Sprintf(`apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
dns:
  imageRepository: %s
  imageTag: %s
`, repository, tag)
}
