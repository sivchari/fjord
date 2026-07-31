package kind

import (
	"context"
	"fmt"

	kindcluster "sigs.k8s.io/kind/pkg/cluster"

	"github.com/sivchari/fjord/internal/logger"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// provider implements clusterprovider.Provider on top of kind's docker node
// provider.
type provider struct {
	inner *kindcluster.Provider
}

var _ clusterprovider.Provider = (*provider)(nil)

// NewProvider returns a clusterprovider.Provider backed by kind's docker
// node provider, logging through logger.
func NewProvider(logger logger.Logger) clusterprovider.Provider {
	return &provider{
		inner: kindcluster.NewProvider(
			kindcluster.ProviderWithLogger(logAdapter{inner: logger}),
			kindcluster.ProviderWithDocker(),
		),
	}
}

// CreateCluster implements clusterprovider.Provider. kind's own Create call
// predates context.Context and cannot be cancelled once started, but ctx is
// honored for the inotify limits raised against the cluster's nodes
// afterwards.
func (p *provider) CreateCluster(ctx context.Context, name string, opts clusterprovider.CreateOptions) error {
	createOpts := []kindcluster.CreateOption{
		kindcluster.CreateWithDisplayUsage(false),
		kindcluster.CreateWithDisplaySalutation(false),
	}

	if opts.NodeImage != "" {
		createOpts = append(createOpts, kindcluster.CreateWithNodeImage(opts.NodeImage))
	}

	if opts.Config != nil {
		createOpts = append(createOpts, kindcluster.CreateWithV1Alpha4Config(ToV1Alpha4(opts.Config)))
	}

	if opts.WaitForReady > 0 {
		createOpts = append(createOpts, kindcluster.CreateWithWaitForReady(opts.WaitForReady))
	}

	if err := p.inner.Create(name, createOpts...); err != nil {
		return fmt.Errorf("create cluster %q: %w", name, err)
	}

	return p.raiseInotifyLimits(ctx, name)
}

// DeleteCluster implements clusterprovider.Provider. kind's own Delete call
// predates context.Context, so ctx is accepted for interface conformance but
// cannot be passed down.
func (p *provider) DeleteCluster(_ context.Context, name, kubeconfigPath string) error {
	if err := p.inner.Delete(name, kubeconfigPath); err != nil {
		return fmt.Errorf("delete cluster %q: %w", name, err)
	}

	return nil
}

// ListClusters implements clusterprovider.Provider.
func (p *provider) ListClusters() ([]string, error) {
	names, err := p.inner.List()
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	return names, nil
}

// KubeConfig implements clusterprovider.Provider.
func (p *provider) KubeConfig(name string) (string, error) {
	kubeconfig, err := p.inner.KubeConfig(name, false)
	if err != nil {
		return "", fmt.Errorf("get kubeconfig for cluster %q: %w", name, err)
	}

	return kubeconfig, nil
}
