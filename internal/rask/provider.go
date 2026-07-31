package rask

import (
	"context"
	"fmt"
	"path/filepath"

	raskcluster "github.com/sivchari/rask/pkg/cluster"

	"github.com/sivchari/fjord/internal/logger"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// provider implements clusterprovider.Provider on top of rask's Go API
// (github.com/sivchari/rask/pkg/cluster).
type provider struct {
	inner  *raskcluster.Provider
	logger logger.Logger
}

var _ clusterprovider.Provider = (*provider)(nil)

// NewProvider returns a clusterprovider.Provider backed by rask, storing
// cluster state under rask's own default home directory and logging
// warnings for Config fields rask has no equivalent for through logger.
func NewProvider(logger logger.Logger) (clusterprovider.Provider, error) {
	inner, err := raskcluster.NewProvider("")
	if err != nil {
		return nil, fmt.Errorf("new rask provider: %w", err)
	}

	return &provider{inner: inner, logger: logger}, nil
}

// CreateCluster implements clusterprovider.Provider.
func (p *provider) CreateCluster(ctx context.Context, name string, opts clusterprovider.CreateOptions) error {
	if opts.NodeImage != "" {
		return fmt.Errorf("rask provider does not support CreateOptions.NodeImage (got %q); select node components via CreateOptions.ComponentDir instead", opts.NodeImage)
	}

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

	if c.HostPort != 0 {
		// TODO(rask CLI wiring): rask's hostproc runtime shares the host
		// network namespace, so a NodePort Service is already reachable on
		// the host directly and no mapping is needed here. What is
		// unresolved is the port *number* the fjord CLI advertises to the
		// user (kind: the HostPort the caller requested; rask: the
		// NodePort itself, e.g. 30080) — that story belongs to whichever
		// CLI code selects the rask provider, not to this package.
		p.logger.Warnf("HostPort %d is not applicable to the rask provider: a NodePort Service is already reachable on the host directly under rask's hostproc runtime, no port mapping is configured", c.HostPort)
	}

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

// DeleteCluster implements clusterprovider.Provider. kubeconfigPath is
// kind-specific: kind removes the cluster's context from that file as part
// of deleting it. rask writes each cluster's kubeconfig to its own
// per-cluster file (never shared across clusters), so there is nothing to
// remove it from; kubeconfigPath is accepted for interface conformance but
// ignored.
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
// ExportOptions (context named exactly name), not a "kind-<name>"-style
// rename: the Provider interface only promises "the kubeconfig content for
// the cluster named name", and fjord's own "kind-<name>" context assumption
// (cli/create.go's user-facing message, cli/principal.go's --context
// default) is CLI-level code specific to the kind provider today, not part
// of this interface's contract. Wiring the CLI to select between providers
// — and to expect the right context name from whichever one it picked — is
// out of scope here.
func (p *provider) KubeConfig(name string) (string, error) {
	data, err := p.inner.KubeConfig(name, raskcluster.ExportOptions{})
	if err != nil {
		return "", fmt.Errorf("get kubeconfig for cluster %q: %w", name, err)
	}

	return string(data), nil
}
