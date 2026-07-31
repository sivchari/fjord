package cli

import (
	"errors"
	"fmt"

	"github.com/sivchari/fjord/internal/cluster"
	"github.com/sivchari/fjord/internal/kind"
	"github.com/sivchari/fjord/internal/logger"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
	"github.com/sivchari/fjord/internal/rask"
)

const (
	// providerKind selects the kind (docker) cluster backend, fjord's
	// default and, for now, the only one supported on macOS.
	providerKind = "kind"
	// providerRask selects the rask cluster backend.
	providerRask = "rask"

	// providerFlagUsage is shared by every command exposing --provider.
	providerFlagUsage = "cluster backend to use: kind or rask (default: kind)"
)

// errUnknownProvider is wrapped with the offending --provider value by
// newClusterProvider when it names neither backend.
var errUnknownProvider = errors.New("unknown provider")

// newClusterProvider constructs the clusterprovider.Provider backend named
// name ("kind" or "rask"), logging through logger.
func newClusterProvider(name string, logger logger.Logger) (clusterprovider.Provider, error) {
	switch name {
	case providerKind:
		return kind.NewProvider(logger), nil
	case providerRask:
		provider, err := rask.NewProvider(logger)
		if err != nil {
			return nil, fmt.Errorf("new rask provider: %w", err)
		}

		return provider, nil
	default:
		return nil, fmt.Errorf("%w %q: want one of %q, %q", errUnknownProvider, name, providerKind, providerRask)
	}
}

// clusterContextName returns the kubeconfig context name provider's
// KubeConfig uses for the cluster named name: "kind-<name>", matching
// kind's own default context naming, or "<name>" for rask, which names
// contexts exactly the cluster name (see rask's internal/bootstrap/pki.go).
func clusterContextName(provider, name string) string {
	if provider == providerRask {
		return name
	}

	return "kind-" + name
}

// resolveAgentHostPort returns the host port to reach fjord-agent's fake STS
// API on for the selected provider: hostPort when the caller explicitly set
// --agent-host-port (flagChanged) or the provider is kind (published via a
// port mapping fjord configures), or cluster.AgentNodePort for rask, whose
// hostproc runtime shares the host network namespace and so needs no
// mapping.
func resolveAgentHostPort(flagChanged bool, provider string, hostPort int32) int32 {
	if flagChanged || provider != providerRask {
		return hostPort
	}

	return cluster.AgentNodePort
}
