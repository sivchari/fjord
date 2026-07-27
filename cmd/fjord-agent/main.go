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
	"os/exec"
	"os/signal"
	"strings"
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

// imdsLinkLocalAddr is the EC2 Instance Metadata Service's well-known
// link-local address. serveIMDS adds it to the node's loopback interface
// before listening on it, so pods reach fjord's IMDS the same way they
// would reach the real one on an EC2 instance.
const imdsLinkLocalAddr = "169.254.169.254"

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
// requests to IAM principals via the internal/agent principal registry, and
// (unless disabled with --injector-port 0) the IRSA injector webhook that
// points IRSA pods' AWS SDK at that same fake STS.
func newServeAPICmd() *cobra.Command {
	opts := &apiServeOptions{}

	cmd := &cobra.Command{
		Use:   "api",
		Short: "Serve the fake STS API and the IRSA injector webhook",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveAPI(cmd, opts)
		},
	}

	cmd.Flags().IntVar(&opts.port, "port", 8080, "port to listen on for the fake STS API")
	cmd.Flags().IntVar(&opts.injectorPort, "injector-port", 8443,
		"port to listen on (TLS) for the IRSA injector webhook; 0 disables it")
	cmd.Flags().StringVar(&opts.tlsCertFile, "tls-cert-file", "/etc/fjord/tls/tls.crt", "TLS certificate file for the injector webhook")
	cmd.Flags().StringVar(&opts.tlsKeyFile, "tls-key-file", "/etc/fjord/tls/tls.key", "TLS private key file for the injector webhook")
	cmd.Flags().StringVar(&opts.stsEndpoint, "sts-endpoint", agent.DefaultSTSEndpoint,
		"AWS_ENDPOINT_URL_STS value the injector webhook injects into IRSA pods")

	return cmd
}

// apiServeOptions carries `serve api`'s flag values.
type apiServeOptions struct {
	port         int
	injectorPort int
	tlsCertFile  string
	tlsKeyFile   string
	stsEndpoint  string
}

// serveAPI builds an in-cluster Kubernetes client, wires it to the fake STS
// API server and (unless opts.injectorPort is 0) the IRSA injector webhook,
// and serves both until the process receives a termination signal.
func serveAPI(cmd *cobra.Command, opts *apiServeOptions) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	podIdentityStore := agent.NewConfigMapPodIdentityStore(clientset)

	server := agent.NewServer(agent.NewSecretPrincipalStore(clientset), agent.WithPodIdentity(clientset, podIdentityStore))

	servers := []managedServer{
		{
			server: &http.Server{
				Addr:              fmt.Sprintf(":%d", opts.port),
				Handler:           server.Handler(),
				ReadHeaderTimeout: shutdownTimeout,
			},
		},
	}

	if opts.injectorPort != 0 {
		injector := agent.NewInjector(clientset, opts.stsEndpoint, podIdentityStore)
		servers = append(servers, managedServer{
			server: &http.Server{
				Addr:              fmt.Sprintf(":%d", opts.injectorPort),
				Handler:           injector.Handler(),
				ReadHeaderTimeout: shutdownTimeout,
			},
			tlsCertFile: opts.tlsCertFile,
			tlsKeyFile:  opts.tlsKeyFile,
		})
	}

	return runUntilSignal(cmd, servers...)
}

// managedServer pairs an *http.Server with the TLS certificate/key files to
// serve it with, if any (a plain HTTP server leaves both empty).
type managedServer struct {
	server      *http.Server
	tlsCertFile string
	tlsKeyFile  string
}

// serve runs m's server, over TLS if m carries certificate/key files. The
// returned error still satisfies errors.Is(err, http.ErrServerClosed) after
// a graceful Shutdown.
func (m managedServer) serve() error {
	var err error
	if m.tlsCertFile != "" {
		err = m.server.ListenAndServeTLS(m.tlsCertFile, m.tlsKeyFile)
	} else {
		err = m.server.ListenAndServe()
	}

	if err != nil {
		return fmt.Errorf("listen on %s: %w", m.server.Addr, err)
	}

	return nil
}

// runUntilSignal starts every server in servers and blocks until the
// process receives a termination signal or one of them exits with an error,
// then shuts all of them down gracefully.
func runUntilSignal(cmd *cobra.Command, servers ...managedServer) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, len(servers))

	for _, s := range servers {
		go func() { errCh <- s.serve() }()
	}

	select {
	case err := <-errCh:
		if shutdownErr := shutdownServers(servers); shutdownErr != nil {
			return shutdownErr
		}

		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	case <-ctx.Done():
		return shutdownServers(servers)
	}
}

// shutdownServers gracefully shuts down every server in servers, waiting up
// to shutdownTimeout for in-flight requests to finish.
func shutdownServers(servers []managedServer) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var errs []error

	for _, s := range servers {
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown server %s: %w", s.server.Addr, err))
		}
	}

	return errors.Join(errs...)
}

// newServeIMDSCmd serves the fake EC2 instance metadata service pods use
// to fetch temporary node-role credentials, as if fjord's kind nodes were
// real EC2 instances.
func newServeIMDSCmd() *cobra.Command {
	opts := &imdsServeOptions{}

	cmd := &cobra.Command{
		Use:   "imds",
		Short: "Serve the fake EC2 instance metadata service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveIMDS(cmd, opts)
		},
	}

	cmd.Flags().IntVar(&opts.port, "port", 80, "port to listen on, bound to "+imdsLinkLocalAddr)
	cmd.Flags().StringVar(&opts.nodeRoleName, "node-role-name", agent.DefaultNodeRoleName,
		"IAM role name IMDS advertises as the node's instance role")

	return cmd
}

// imdsServeOptions carries `serve imds`'s flag values.
type imdsServeOptions struct {
	port         int
	nodeRoleName string
}

// serveIMDS adds imdsLinkLocalAddr to the node's loopback interface, builds
// an in-cluster Kubernetes client, and serves the fake IMDS on that address
// until the process receives a termination signal.
func serveIMDS(cmd *cobra.Command, opts *imdsServeOptions) error {
	if err := ensureLinkLocalAddr(cmd.Context()); err != nil {
		return err
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	imds, err := agent.NewIMDS(agent.NewSecretPrincipalStore(clientset), opts.nodeRoleName)
	if err != nil {
		return fmt.Errorf("create imds server: %w", err)
	}

	server := managedServer{
		server: &http.Server{
			Addr:              fmt.Sprintf("%s:%d", imdsLinkLocalAddr, opts.port),
			Handler:           imds.Handler(),
			ReadHeaderTimeout: shutdownTimeout,
		},
	}

	return runUntilSignal(cmd, server)
}

// ensureLinkLocalAddr adds imdsLinkLocalAddr to the node's loopback
// interface, so listening on it emulates the address every AWS SDK's
// default credential chain and IMDS client falls back to (matching how the
// official eks-pod-identity-agent binds its own link-local address). It is
// idempotent: on a container restart the address is already present, which
// `ip addr add` reports with a message that varies by iproute2 version
// ("File exists" or "Address already assigned"); neither is fatal.
func ensureLinkLocalAddr(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "ip", "addr", "add", imdsLinkLocalAddr+"/32", "dev", "lo")

	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	msg := string(out)
	if strings.Contains(msg, "File exists") || strings.Contains(msg, "already assigned") {
		return nil
	}

	return fmt.Errorf("add %s to lo: %w: %s", imdsLinkLocalAddr, err, out)
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
