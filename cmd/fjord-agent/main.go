// Command fjord-agent runs the server components backing a fjord cluster's
// EKS emulation: the fake STS/IMDS APIs and the Kubernetes authenticator
// webhook.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// version is injected by goreleaser via -ldflags "-X main.version=...".
var version string

func main() {
	cmd := newRootCmd()
	if version != "" {
		cmd.Version = version
	}

	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln("Error:", err.Error())
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "fjord-agent",
		Short:         "fjord-agent runs the server components backing a fjord cluster's EKS emulation",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.AddCommand(newServeCmd())

	return cmd
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve a fjord-agent component",
	}

	cmd.AddCommand(newServeAPICmd())
	cmd.AddCommand(newServeIMDSCmd())
	cmd.AddCommand(newServeAuthenticatorCmd())

	return cmd
}

// newServeAPICmd serves the fake STS API used to resolve SigV4-signed
// requests to IAM principals via the internal/agent principal registry.
func newServeAPICmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "api",
		Short: "Serve the fake STS API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return blockUnimplemented(cmd, "api", port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 8443, "port to listen on")

	return cmd
}

// newServeIMDSCmd serves the fake EC2 instance metadata service pods use
// to fetch temporary IRSA-style credentials.
func newServeIMDSCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "imds",
		Short: "Serve the fake EC2 instance metadata service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return blockUnimplemented(cmd, "imds", port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 80, "port to listen on")

	return cmd
}

// newServeAuthenticatorCmd serves the Kubernetes API server's
// authentication webhook, mapping resolved IAM principals to Kubernetes
// users and groups.
func newServeAuthenticatorCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "authenticator",
		Short: "Serve the Kubernetes API server authentication webhook",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return blockUnimplemented(cmd, "authenticator", port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 9443, "port to listen on")

	return cmd
}

// blockUnimplemented reports that mode is not implemented yet and blocks
// until the process receives a termination signal. It is a placeholder
// until the corresponding internal/agent server is wired in.
func blockUnimplemented(cmd *cobra.Command, mode string, port int) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "fjord-agent serve %s: not implemented yet (would listen on port %d)\n", mode, port)

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	return nil
}
