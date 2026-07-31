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
	// ConfigFilePath is the path, inside the cluster, to the
	// authentication-token-webhook-config-file.
	ConfigFilePath string
	// VolumeMountPath is the in-cluster directory the config file's
	// ExtraMounts entry is delivered to; the rask provider matches it
	// against Mount.ContainerPath to locate the owning mount.
	VolumeMountPath string
}

// Config is the substrate-neutral cluster configuration fjord exposes.
type Config struct {
	// Name is the cluster name.
	Name string
	// KubeVersion is the Kubernetes version the cluster runs (e.g.
	// "v1.33.13"), matching the EKS-D release CreateOptions.ComponentDir was
	// materialized from.
	KubeVersion string
	// CoreDNSImageRepository overrides the CoreDNS image repository. Leave
	// empty to use rask's default.
	CoreDNSImageRepository string
	// CoreDNSImageTag overrides the CoreDNS image tag. Leave empty to use
	// rask's default.
	CoreDNSImageTag string
	// ExtraMounts are host directories mounted into the control-plane node,
	// used to deliver the authenticator's static pod manifest, webhook
	// config, and TLS material before the API server starts.
	ExtraMounts []Mount
	// AuthWebhook, if non-nil, configures the API server to authenticate
	// tokens via fjord's authentication webhook.
	AuthWebhook *AuthWebhook
}
