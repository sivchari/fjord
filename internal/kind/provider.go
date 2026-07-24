package kind

import (
	"fmt"
	"time"

	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/log"
)

// Provider is the subset of kind cluster operations fjord uses.
type Provider interface {
	// CreateCluster provisions and starts a cluster named name.
	CreateCluster(name string, opts CreateOptions) error
	// DeleteCluster tears down the cluster named name. kubeconfigPath, if
	// non-empty, is also removed from that kubeconfig file.
	DeleteCluster(name, kubeconfigPath string) error
	// ListClusters returns the names of all clusters kind knows about.
	ListClusters() ([]string, error)
	// KubeConfig returns the kubeconfig content for the cluster named name.
	KubeConfig(name string) (string, error)
}

// CreateOptions configures Provider.CreateCluster.
type CreateOptions struct {
	// NodeImage overrides the node image used for every node in Config,
	// e.g. to select a specific Kubernetes version.
	NodeImage string
	// Config is the cluster configuration to apply. If nil, kind's default
	// single control-plane configuration is used.
	Config *Config
	// WaitForReady is the maximum time to wait for the control-plane node
	// to be ready. Zero disables waiting.
	WaitForReady time.Duration
}

// provider implements Provider on top of kind's docker node provider.
type provider struct {
	inner *kindcluster.Provider
}

// NewProvider returns a Provider backed by kind's docker node provider,
// logging through logger.
func NewProvider(logger log.Logger) Provider {
	return &provider{
		inner: kindcluster.NewProvider(
			kindcluster.ProviderWithLogger(logger),
			kindcluster.ProviderWithDocker(),
		),
	}
}

// CreateCluster implements Provider.
func (p *provider) CreateCluster(name string, opts CreateOptions) error {
	createOpts := []kindcluster.CreateOption{
		kindcluster.CreateWithDisplayUsage(false),
		kindcluster.CreateWithDisplaySalutation(false),
	}

	if opts.NodeImage != "" {
		createOpts = append(createOpts, kindcluster.CreateWithNodeImage(opts.NodeImage))
	}

	if opts.Config != nil {
		createOpts = append(createOpts, kindcluster.CreateWithV1Alpha4Config(opts.Config.ToV1Alpha4()))
	}

	if opts.WaitForReady > 0 {
		createOpts = append(createOpts, kindcluster.CreateWithWaitForReady(opts.WaitForReady))
	}

	if err := p.inner.Create(name, createOpts...); err != nil {
		return fmt.Errorf("create cluster %q: %w", name, err)
	}

	return nil
}

// DeleteCluster implements Provider.
func (p *provider) DeleteCluster(name, kubeconfigPath string) error {
	if err := p.inner.Delete(name, kubeconfigPath); err != nil {
		return fmt.Errorf("delete cluster %q: %w", name, err)
	}

	return nil
}

// ListClusters implements Provider.
func (p *provider) ListClusters() ([]string, error) {
	names, err := p.inner.List()
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	return names, nil
}

// KubeConfig implements Provider.
func (p *provider) KubeConfig(name string) (string, error) {
	kubeconfig, err := p.inner.KubeConfig(name, false)
	if err != nil {
		return "", fmt.Errorf("get kubeconfig for cluster %q: %w", name, err)
	}

	return kubeconfig, nil
}
