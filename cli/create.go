package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/cluster"
	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/kind"
	"github.com/sivchari/fjord/internal/nodeimage"
	"github.com/sivchari/fjord/internal/pki"
)

const (
	defaultClusterName = "fjord"
	// defaultAgentHostPort is the default host port fjord-agent's fake STS
	// API is published on.
	defaultAgentHostPort = 48080
)

func newCreateCmd(logger log.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a resource",
	}
	cmd.AddCommand(newCreateClusterCmd(logger))
	cmd.AddCommand(newCreatePrincipalCmd(logger))

	return cmd
}

// createClusterOptions carries the create cluster flag values.
type createClusterOptions struct {
	eksVersion string
	name       string
	buildLocal bool
	wait       time.Duration
	enableAuth bool
	agentImage string
	hostPort   int32
}

func newCreateClusterCmd(logger log.Logger) *cobra.Command {
	opts := &createClusterOptions{}

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Create an EKS-compatible local cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCreateCluster(cmd.Context(), logger, opts)
		},
	}

	cmd.Flags().StringVar(&opts.eksVersion, "eks-version", "", "EKS version to emulate (default: latest supported)")
	cmd.Flags().StringVar(&opts.name, "name", defaultClusterName, "cluster name")
	cmd.Flags().BoolVar(&opts.buildLocal, "build-local", false, "build the node image locally instead of pulling the prebuilt image")
	cmd.Flags().DurationVar(&opts.wait, "wait", 2*time.Minute, "wait for the control plane to be ready (0 to disable)")
	cmd.Flags().BoolVar(&opts.enableAuth, "enable-auth", true, "deploy fjord-agent for EKS-compatible AWS authentication")
	cmd.Flags().StringVar(&opts.agentImage, "agent-image", "", "fjord-agent image to deploy (default: "+agentImageRepository+":<version>, or a locally built image with --build-local)")
	cmd.Flags().Int32Var(&opts.hostPort, "agent-host-port", defaultAgentHostPort, "host port fjord-agent's fake STS API is published on")

	return cmd
}

func runCreateCluster(ctx context.Context, logger log.Logger, opts *createClusterOptions) error {
	eksVersion := opts.eksVersion
	if eksVersion == "" {
		eksVersion = latestVersion(eksd.SupportedVersions())
	}

	release, err := eksd.Resolve(eksVersion)
	if err != nil {
		return fmt.Errorf("resolve eks version: %w", err)
	}

	image, err := resolveNodeImage(ctx, logger, opts, release)
	if err != nil {
		return err
	}

	coreDNSRepo, coreDNSTag := coreDNSKubeadmRepository(release.CoreDNSImage)

	logger.V(0).Infof("Creating cluster %q (EKS %s / %s) ...", opts.name, release.EKSVersion, release.KubeVersion)

	provider := kind.NewProvider(logger)

	err = provider.CreateCluster(opts.name, kind.CreateOptions{
		NodeImage: image,
		Config: &kind.Config{
			Name:                   opts.name,
			KubeVersion:            release.KubeVersion,
			CoreDNSImageRepository: coreDNSRepo,
			CoreDNSImageTag:        coreDNSTag,
			HostPort:               agentHostPort(opts.enableAuth, opts.hostPort),
		},
		WaitForReady: opts.wait,
	})
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	client, err := clusterClient(provider, opts.name)
	if err != nil {
		return err
	}

	logger.V(0).Info("Applying EKS default state (gp2 StorageClass) ...")

	if err := cluster.EnsureDefaultStorageClass(ctx, client); err != nil {
		return fmt.Errorf("ensure default storage class: %w", err)
	}

	if opts.enableAuth {
		if err := deployAgent(ctx, logger, provider, client, opts); err != nil {
			return err
		}
	}

	logger.V(0).Infof("Cluster %q is ready. Set kubectl context to \"kind-%s\".", opts.name, opts.name)

	return nil
}

// resolveNodeImage returns the node image tag to use for release: the
// prebuilt floating tag, or a freshly built local image when
// opts.buildLocal is set.
func resolveNodeImage(ctx context.Context, logger log.Logger, opts *createClusterOptions, release *eksd.Release) (string, error) {
	if !opts.buildLocal {
		return nodeimage.FloatingTag(release), nil
	}

	logger.V(0).Infof("Building node image for EKS %s (%s) ...", release.EKSVersion, release.KubeVersion)

	image, err := nodeimage.Build(ctx, release, buildArch(), logger)
	if err != nil {
		return "", fmt.Errorf("build node image: %w", err)
	}

	return image, nil
}

// agentHostPort returns the kind ExtraPortMappings host port to publish
// fjord-agent's fake STS API on, or 0 (no mapping) when auth is disabled.
func agentHostPort(enableAuth bool, hostPort int32) int32 {
	if !enableAuth {
		return 0
	}

	return hostPort
}

// clusterClient builds a Kubernetes client for the cluster named name via
// provider's kubeconfig.
func clusterClient(provider kind.Provider, name string) (kubernetes.Interface, error) {
	kubeconfig, err := provider.KubeConfig(name)
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig: %w", err)
	}

	client, err := cluster.NewClient(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}

	return client, nil
}

// deployAgent builds (if requested) and deploys fjord-agent to the cluster,
// giving it EKS-compatible AWS authentication.
func deployAgent(ctx context.Context, logger log.Logger, provider kind.Provider, client kubernetes.Interface, opts *createClusterOptions) error {
	image := opts.agentImage
	if image == "" {
		image = defaultAgentImage()
	}

	if opts.buildLocal && opts.agentImage == "" {
		logger.V(0).Infof("Building fjord-agent image %s (%s) ...", image, buildArch())

		if err := buildAgentImage(ctx, image, buildArch(), true, os.Stderr, os.Stderr); err != nil {
			return fmt.Errorf("build agent image: %w", err)
		}

		logger.V(0).Infof("Loading fjord-agent image %s into cluster %q ...", image, opts.name)

		if err := provider.LoadDockerImage(opts.name, image); err != nil {
			return fmt.Errorf("load agent image: %w", err)
		}
	}

	ca, err := pki.NewCA("fjord")
	if err != nil {
		return fmt.Errorf("generate fjord CA: %w", err)
	}

	logger.V(0).Infof("Deploying fjord-agent (%s) ...", image)

	if err := cluster.EnsureAgent(ctx, client, image, true); err != nil {
		return fmt.Errorf("ensure agent: %w", err)
	}

	logger.V(0).Info("Deploying IRSA support (pod-identity-webhook + fjord injector) ...")

	if err := cluster.EnsureIRSA(ctx, client, ca, cluster.DefaultPodIdentityWebhookImage); err != nil {
		return fmt.Errorf("ensure irsa: %w", err)
	}

	return nil
}
