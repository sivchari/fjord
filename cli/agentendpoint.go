package cli

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sivchari/fjord/internal/cluster"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// agentReadyTimeout bounds how long agentEndpoint waits for fjord-agent to
// start answering. It is minutes rather than seconds because on a cluster
// created moments ago the agent's image may still be pulling.
const agentReadyTimeout = 3 * time.Minute

// agentEndpoint returns a base URL a host process can use to reach
// fjord-agent's HTTP API for the cluster named name, along with a stop
// function the caller must invoke when done.
//
// It tunnels rather than dialling 127.0.0.1:<NodePort> directly. That
// shortcut only works where the host is the node, which is true of rask's
// hostproc runtime and false of vz, where the cluster (and its NodePort)
// live inside a Linux VM. Going through the provider keeps this working on
// both without the caller knowing which is in play.
//
// It returns only once fjord-agent actually answers, because `fjord create`
// deploys the agent without waiting for it: a caller that got the address
// the moment the tunnel bound would send its first request into a tunnel
// with nothing on the far end.
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

	baseURL = "http://" + bound

	if err := waitForAgent(ctx, baseURL); err != nil {
		cancel()

		return "", nil, fmt.Errorf("reach fjord-agent in cluster %q: %w", name, err)
	}

	return baseURL, cancel, nil
}

// waitForAgent polls baseURL until fjord-agent answers or the wait runs out.
//
// Binding the tunnel proves nothing about the far end: it accepts local
// connections and only then tries to reach the NodePort, so an agent that is
// not up yet shows up as a connection closed mid-request rather than as a
// failure to open the tunnel.
func waitForAgent(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, agentReadyTimeout)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error

	for {
		// ListClusters: a request the agent always answers, with no side
		// effects and no credentials.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/clusters", http.NoBody)
		if err != nil {
			return fmt.Errorf("build readiness request: %w", err)
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()

			return nil
		}

		lastErr = err

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("fjord-agent did not answer at %s within %s: %w", baseURL, agentReadyTimeout, lastErr)
		}
	}
}
