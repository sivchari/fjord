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
	// ComponentDir points at a directory of pre-extracted EKS-D Kubernetes
	// component binaries rask runs the cluster's nodes from, matching the
	// version selected by Config.KubeVersion. Left empty, rask boots
	// upstream Kubernetes instead (used on darwin, where rask's vz
	// substrate does not yet support ComponentDir).
	ComponentDir string
	// Config is the cluster configuration to apply. If nil, the
	// implementation's default single control-plane configuration is used.
	Config *Config
	// WaitForReady is the maximum time to wait for the control-plane node
	// to be ready. Zero disables waiting.
	WaitForReady time.Duration
}
