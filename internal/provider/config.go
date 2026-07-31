package provider

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

// Config is the substrate-neutral cluster configuration fjord exposes.
type Config struct {
	// Name is the cluster name.
	Name string
	// KubeVersion is the Kubernetes version the cluster runs (e.g.
	// "v1.33.13"). It selects the kubeadm config API version for patches
	// and must match the node image (kind: NodeImage in CreateOptions;
	// rask: paired with a component directory).
	KubeVersion string
	// CoreDNSImageRepository overrides the CoreDNS image repository via a
	// kubeadm ClusterConfiguration patch. Leave empty to use the
	// implementation's default.
	CoreDNSImageRepository string
	// CoreDNSImageTag overrides the CoreDNS image tag via a kubeadm
	// ClusterConfiguration patch. Leave empty to use the implementation's
	// default.
	CoreDNSImageTag string
	// HostPort, if nonzero, publishes fjord-agent's NodePort Service on
	// this host port, so callers outside the cluster can reach its fake STS
	// API. Zero adds no port mapping.
	HostPort int32
	// ExtraMounts are host directories mounted into the control-plane node,
	// used to deliver the authenticator's static pod manifest, webhook
	// config, and TLS material before the API server starts.
	ExtraMounts []Mount
	// AuthWebhook, if non-nil, configures the API server to authenticate
	// tokens via fjord's authentication webhook.
	AuthWebhook *AuthWebhook
}
