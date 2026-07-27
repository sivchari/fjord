// Command fjord-agent runs the server components backing a fjord cluster's
// EKS emulation: the fake STS/IMDS APIs and the Kubernetes authenticator
// webhook.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sivchari/fjord/internal/agent"
)

// shutdownTimeout bounds how long serveAPI waits for in-flight requests to
// finish once it receives a termination signal.
const shutdownTimeout = 5 * time.Second

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
			return serveAPI(cmd, port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 8080, "port to listen on")

	return cmd
}

// serveAPI builds an in-cluster Kubernetes client, wires it to the fake STS
// API server, and serves it on port until the process receives a
// termination signal.
func serveAPI(cmd *cobra.Command, port int) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	server := agent.NewServer(agent.NewSecretPrincipalStore(clientset))

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           server.Handler(),
		ReadHeaderTimeout: shutdownTimeout,
	}

	return runUntilSignal(cmd, httpServer)
}

// runUntilSignal starts httpServer and blocks until the process receives a
// termination signal, then shuts httpServer down gracefully.
func runUntilSignal(cmd *cobra.Command, httpServer *http.Server) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown server: %w", err)
		}

		return nil
	}
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
