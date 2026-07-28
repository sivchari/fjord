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

// Mount is a host directory mounted into the control-plane node container.
type Mount struct {
	HostPath      string
	ContainerPath string
}

// AuthWebhook configures the API server to call fjord's authentication token
// webhook. The webhook config file and its directory are delivered to the
// node via Config.ExtraMounts.
type AuthWebhook struct {
	// ConfigFilePath is the path, inside the API server container, to the
	// authentication-token-webhook-config-file.
	ConfigFilePath string
	// VolumeName names the extra volume mounting the config file's directory
	// into the API server static pod.
	VolumeName string
	// VolumeHostPath is that directory's path on the node, and
	// VolumeMountPath where it mounts inside the API server container.
	VolumeHostPath  string
	VolumeMountPath string
}

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
	// ExtraMounts are host directories mounted into the control-plane node,
	// used to deliver the authenticator's static pod manifest, webhook
	// config, and TLS material before the API server starts.
	ExtraMounts []Mount
	// AuthWebhook, if non-nil, configures the API server to authenticate
	// tokens via fjord's authentication webhook.
	AuthWebhook *AuthWebhook
}

// ToV1Alpha4 builds the kind v1alpha4.Cluster equivalent of c. v0 only
// supports a single control-plane node, so a node is defined only when a port
// mapping or extra mount requires one; otherwise kind applies its own
// single-node default.
func (c *Config) ToV1Alpha4() *v1alpha4.Cluster {
	cluster := &v1alpha4.Cluster{
		Name: c.Name,
	}

	if patch := c.clusterConfigurationPatch(); patch != "" {
		cluster.KubeadmConfigPatches = append(cluster.KubeadmConfigPatches, patch)
	}

	if node := c.controlPlaneNode(); node != nil {
		cluster.Nodes = []v1alpha4.Node{*node}
	}

	return cluster
}

// controlPlaneNode returns the control-plane node definition when a port
// mapping or extra mount requires one, or nil to let kind use its default.
func (c *Config) controlPlaneNode() *v1alpha4.Node {
	if c.HostPort == 0 && len(c.ExtraMounts) == 0 {
		return nil
	}

	node := &v1alpha4.Node{Role: v1alpha4.ControlPlaneRole}

	if c.HostPort != 0 {
		node.ExtraPortMappings = []v1alpha4.PortMapping{
			{ContainerPort: agentNodePort, HostPort: c.HostPort},
		}
	}

	for _, m := range c.ExtraMounts {
		node.ExtraMounts = append(node.ExtraMounts, v1alpha4.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
		})
	}

	return node
}

// clusterConfigurationPatch builds a single kubeadm ClusterConfiguration
// patch carrying every ClusterConfiguration-level override fjord needs
// (CoreDNS image and the API server authentication webhook). Combining them
// into one patch avoids depending on the merge order of multiple patches. It
// returns "" when no override applies.
func (c *Config) clusterConfigurationPatch() string {
	dns := c.CoreDNSImageRepository != "" || c.CoreDNSImageTag != ""
	if !dns && c.AuthWebhook == nil {
		return ""
	}

	var b strings.Builder

	fmt.Fprintf(&b, "apiVersion: %s\nkind: ClusterConfiguration\n", kubeadmAPIVersion(c.KubeVersion))

	if dns {
		fmt.Fprintf(&b, "dns:\n  imageRepository: %s\n  imageTag: %s\n", c.CoreDNSImageRepository, c.CoreDNSImageTag)
	}

	if c.AuthWebhook != nil {
		b.WriteString(c.apiServerSection())
	}

	return b.String()
}

// apiServerSection renders the apiServer stanza of the ClusterConfiguration
// patch wiring up the authentication token webhook. The extraArgs shape
// differs between kubeadm API versions: v1beta3 uses a map, v1beta4 a list of
// {name, value}.
func (c *Config) apiServerSection() string {
	w := c.AuthWebhook

	var b strings.Builder

	b.WriteString("apiServer:\n")

	if kubeMinor(c.KubeVersion) >= 36 {
		b.WriteString("  extraArgs:\n")
		fmt.Fprintf(&b, "    - name: authentication-token-webhook-config-file\n      value: %s\n", w.ConfigFilePath)
	} else {
		fmt.Fprintf(&b, "  extraArgs:\n    authentication-token-webhook-config-file: %s\n", w.ConfigFilePath)
	}

	fmt.Fprintf(&b, "  extraVolumes:\n    - name: %s\n      hostPath: %s\n      mountPath: %s\n      readOnly: true\n      pathType: DirectoryOrCreate\n",
		w.VolumeName, w.VolumeHostPath, w.VolumeMountPath)

	return b.String()
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
