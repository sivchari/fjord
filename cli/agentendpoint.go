package cli

import (
	"context"
	"fmt"

	"github.com/sivchari/fjord/internal/cluster"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// agentEndpoint returns a base URL a host process can use to reach
// fjord-agent's HTTP API for the cluster named name, along with a stop
// function the caller must invoke when done.
//
// It tunnels rather than dialling 127.0.0.1:<NodePort> directly. That
// shortcut only works where the host is the node, which is true of rask's
// hostproc runtime and false of vz, where the cluster (and its NodePort)
// live inside a Linux VM. Going through the provider keeps this working on
// both without the caller knowing which is in play.
func agentEndpoint(ctx context.Context, provider clusterprovider.Provider, name string) (baseURL string, stop func(), err error) {
	ctx, cancel := context.WithCancel(ctx)

	remote := fmt.Sprintf("127.0.0.1:%d", cluster.AgentNodePort)

	// Port 0: the OS picks a free port and reports which, so this never
	// races another process for a port it reserved and released.
	bound, err := provider.PortForward(ctx, name, cluster.AgentLoopbackHost+":0", remote)
	if err != nil {
		cancel()

		return "", nil, fmt.Errorf("reach fjord-agent in cluster %q: %w", name, err)
	}

	return "http://" + bound, cancel, nil
}
