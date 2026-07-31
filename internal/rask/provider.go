package rask

import (
	"context"
	"fmt"
	"path/filepath"

	raskcluster "github.com/sivchari/rask/pkg/cluster"

	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// provider implements clusterprovider.Provider on top of rask's Go API
// (github.com/sivchari/rask/pkg/cluster).
type provider struct {
	inner *raskcluster.Provider
}

var _ clusterprovider.Provider = (*provider)(nil)

// NewProvider returns a clusterprovider.Provider backed by rask, storing
// cluster state under rask's own default home directory.
func NewProvider() (clusterprovider.Provider, error) {
	inner, err := raskcluster.NewProvider("")
	if err != nil {
		return nil, fmt.Errorf("new rask provider: %w", err)
	}

	return &provider{inner: inner}, nil
}

// CreateCluster implements clusterprovider.Provider.
func (p *provider) CreateCluster(ctx context.Context, name string, opts clusterprovider.CreateOptions) error {
	createOpts := raskcluster.Options{
		Wait:         waitOption(opts.WaitForReady),
		ComponentDir: opts.ComponentDir,
	}

	if opts.Config != nil {
		if err := p.applyConfig(name, opts.Config, &createOpts); err != nil {
			return err
		}
	}

	if _, err := p.inner.Create(ctx, name, createOpts); err != nil {
		return fmt.Errorf("create cluster %q: %w", name, err)
	}

	return nil
}

// applyConfig maps c onto createOpts, mutating it in place.
func (p *provider) applyConfig(name string, c *clusterprovider.Config, createOpts *raskcluster.Options) error {
	coreDNS, err := coreDNSImage(c)
	if err != nil {
		return err
	}

	createOpts.CoreDNSImage = coreDNS

	if len(c.ExtraMounts) > 0 {
		files, err := prebootFiles(c.ExtraMounts)
		if err != nil {
			return err
		}

		createOpts.PrebootFiles = files
	}

	if c.AuthWebhook != nil {
		dataDir := filepath.Join(filepath.Dir(p.inner.KubeConfigPath(name)), "data")

		arg, err := authWebhookArg(c.AuthWebhook, c.ExtraMounts, dataDir)
		if err != nil {
			return err
		}

		createOpts.ExtraAPIServerArgs = append(createOpts.ExtraAPIServerArgs, arg)
	}

	return nil
}

// DeleteCluster implements clusterprovider.Provider. rask writes each
// cluster's kubeconfig to its own per-cluster file (never shared across
// clusters), so there is nothing to remove a context from; kubeconfigPath is
// accepted for interface conformance but ignored.
func (p *provider) DeleteCluster(ctx context.Context, name, _ string) error {
	if err := p.inner.Delete(ctx, name); err != nil {
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

// KubeConfig implements clusterprovider.Provider. It uses rask's default
// ExportOptions, so the kubeconfig context is named exactly name, matching
// cli's own assumption (cli/create.go's user-facing message,
// cli/principal.go's --context default).
func (p *provider) KubeConfig(name string) (string, error) {
	data, err := p.inner.KubeConfig(name, raskcluster.ExportOptions{})
	if err != nil {
		return "", fmt.Errorf("get kubeconfig for cluster %q: %w", name, err)
	}

	return string(data), nil
}
