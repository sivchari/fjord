package provider

import "time"

// Provider is the subset of cluster operations fjord uses, independent of
// the backend that implements them.
type Provider interface {
	// CreateCluster provisions and starts a cluster named name.
	CreateCluster(name string, opts CreateOptions) error
	// DeleteCluster tears down the cluster named name. kubeconfigPath, if
	// non-empty, is also removed from that kubeconfig file.
	DeleteCluster(name, kubeconfigPath string) error
	// ListClusters returns the names of all clusters the provider knows
	// about.
	ListClusters() ([]string, error)
	// KubeConfig returns the kubeconfig content for the cluster named name.
	KubeConfig(name string) (string, error)
	// LoadDockerImage saves the local docker image imageRef and loads it
	// onto every node of the cluster named name.
	LoadDockerImage(name, imageRef string) error
}

// CreateOptions configures Provider.CreateCluster.
type CreateOptions struct {
	// NodeImage is kind-specific: it overrides the node image used for
	// every node in Config, e.g. to select a specific Kubernetes version.
	// Other implementations select the components a node runs differently
	// (rask: Config.KubeVersion plus a directory of extracted EKS-D
	// binaries). Implementations that have no notion of a node image ignore
	// this field.
	NodeImage string
	// Config is the cluster configuration to apply. If nil, the
	// implementation's default single control-plane configuration is used.
	Config *Config
	// WaitForReady is the maximum time to wait for the control-plane node
	// to be ready. Zero disables waiting.
	WaitForReady time.Duration
}
