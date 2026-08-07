package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sivchari/fjord/internal/logger"
)

// newPortForwardCmd builds `fjord port-forward`, which opens a tunnel from
// this machine to fjord-agent's fake AWS APIs and holds it open.
//
// It exists because a host process cannot assume it can reach the cluster's
// own NodePort. That holds only where the host is the node, which is true of
// Linux and false on macOS, where the cluster runs inside a VM -- so
// `aws --endpoint-url http://127.0.0.1:30080` works on one platform and
// fails on the other. Tunnelling works on both.
func newPortForwardCmd(logger logger.Logger) *cobra.Command {
	var name string

	cmd := &cobra.Command{
		Use:   "port-forward",
		Short: "Forward a local port to fjord-agent's fake AWS APIs, until interrupted",
		Long: `Forward a local port to fjord-agent's fake STS and EKS APIs and hold it
open until interrupted.

Point the AWS CLI at the address it prints:

    fjord port-forward --name fjord          # in one terminal
    aws eks list-clusters --endpoint-url http://127.0.0.1:<port>

A host process cannot reach the cluster's NodePort directly on every
platform: on macOS the cluster runs inside a VM, so the port is not on this
machine's loopback. This command works the same either way.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			provider, err := newClusterProvider()
			if err != nil {
				return err
			}

			url, stop, err := agentEndpoint(cmd.Context(), provider, name)
			if err != nil {
				return err
			}
			defer stop()

			// Stdout, not the logger: callers capture this to build an
			// --endpoint-url, so it must not be interleaved with
			// diagnostics on stderr.
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), url)
			logger.V(0).Infof("Forwarding to fjord-agent in cluster %q. Press Ctrl-C to stop.", name)

			return waitForInterrupt(cmd)
		},
	}

	cmd.Flags().StringVar(&name, "name", defaultClusterName, "cluster name")

	return cmd
}

// waitForInterrupt blocks until the command's context is done or the process
// is asked to stop, so the tunnel outlives the call that opened it.
func waitForInterrupt(cmd *cobra.Command) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(signals)

	select {
	case <-signals:
	case <-cmd.Context().Done():
	}

	return nil
}
