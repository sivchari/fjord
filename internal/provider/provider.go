package provider

import (
	"context"
	"time"
)

// Provider is the subset of cluster operations fjord uses, independent of
// the backend that implements them.
type Provider interface {
	// CreateCluster provisions and starts a cluster named name.
	CreateCluster(ctx context.Context, name string, opts CreateOptions) error
	// DeleteCluster tears down the cluster named name. kubeconfigPath, if
	// non-empty, is also removed from that kubeconfig file.
	DeleteCluster(ctx context.Context, name, kubeconfigPath string) error
	// ListClusters returns the names of all clusters the provider knows
	// about.
	ListClusters() ([]string, error)
	// KubeConfig returns the kubeconfig content for the cluster named name.
	KubeConfig(name string) (string, error)
	// LoadDockerImage saves the local docker image imageRef and loads it
	// onto every node of the cluster named name.
	LoadDockerImage(ctx context.Context, name, imageRef string) error
}

// CreateOptions configures Provider.CreateCluster.
type CreateOptions struct {
	// NodeImage is kind-specific: it overrides the node image used for
	// every node in Config, e.g. to select a specific Kubernetes version.
	// Other implementations select the components a node runs differently
	// (rask: ComponentDir, a directory of pre-extracted EKS-D component
	// binaries). Implementations that have no notion of a node image ignore
	// this field.
	NodeImage string
	// ComponentDir is rask-specific: it points at a directory of
	// pre-extracted EKS-D Kubernetes component binaries rask runs the
	// cluster's nodes from, matching the version selected by
	// Config.KubeVersion. Other implementations select the components a
	// node runs differently (kind: NodeImage, a prebuilt node image).
	// Implementations that have no notion of a component directory ignore
	// this field.
	ComponentDir string
	// Config is the cluster configuration to apply. If nil, the
	// implementation's default single control-plane configuration is used.
	Config *Config
	// WaitForReady is the maximum time to wait for the control-plane node
	// to be ready. Zero disables waiting.
	WaitForReady time.Duration
}
