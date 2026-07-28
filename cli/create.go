package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/authn"
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
	eksVersion   string
	name         string
	buildLocal   bool
	wait         time.Duration
	enableAuth   bool
	agentImage   string
	hostPort     int32
	nodeRoleName string
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
	cmd.Flags().StringVar(&opts.nodeRoleName, "node-role-name", agent.DefaultNodeRoleName,
		"IAM role name fjord-imds advertises as the node's instance role")

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

	config, ca, err := buildKindConfig(opts, release)
	if err != nil {
		return err
	}

	logger.V(0).Infof("Creating cluster %q (EKS %s / %s) ...", opts.name, release.EKSVersion, release.KubeVersion)

	provider := kind.NewProvider(logger)

	err = provider.CreateCluster(opts.name, kind.CreateOptions{
		NodeImage:    image,
		Config:       config,
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
		if err := deployAgent(ctx, logger, provider, client, opts, ca); err != nil {
			return err
		}
	}

	logger.V(0).Infof("Cluster %q is ready. Set kubectl context to \"kind-%s\".", opts.name, opts.name)

	return nil
}

// buildKindConfig builds the kind.Config for opts and release. When auth is
// enabled it also generates the fjord CA and stages the authenticator's
// static pod manifest, webhook kubeconfig, and TLS certificate (see
// stageAuthenticator), wiring the result into the returned Config's
// ExtraMounts and AuthWebhook: both must exist before the cluster is
// created, since kind delivers them into the control-plane node as part of
// cluster creation itself, before the API server ever starts. The returned
// CA is nil when auth is disabled; otherwise it is reused later for
// IRSA/pod-identity-webhook's serving certificates, so the webhook
// kubeconfig's trust and every fjord-issued certificate share one root.
func buildKindConfig(opts *createClusterOptions, release *eksd.Release) (*kind.Config, *pki.CA, error) {
	coreDNSRepo, coreDNSTag := coreDNSKubeadmRepository(release.CoreDNSImage)

	config := &kind.Config{
		Name:                   opts.name,
		KubeVersion:            release.KubeVersion,
		CoreDNSImageRepository: coreDNSRepo,
		CoreDNSImageTag:        coreDNSTag,
		HostPort:               agentHostPort(opts.enableAuth, opts.hostPort),
	}

	if !opts.enableAuth {
		return config, nil, nil
	}

	ca, err := pki.NewCA("fjord")
	if err != nil {
		return nil, nil, fmt.Errorf("generate fjord CA: %w", err)
	}

	staged, err := stageAuthenticator(opts, ca)
	if err != nil {
		return nil, nil, err
	}

	config.ExtraMounts = staged.Mounts
	config.AuthWebhook = staged.Webhook

	return config, ca, nil
}

// stageAuthenticator resolves the fjord-agent image opts requests and
// stages the authenticator's static pod manifest, webhook kubeconfig, and
// TLS certificate (issued from ca) for it under a per-cluster host
// directory (see authnStagingDir), clearing any stale staging left by a
// previous cluster of the same name first.
func stageAuthenticator(opts *createClusterOptions, ca *pki.CA) (*authn.StagedAuthn, error) {
	dir, err := authnStagingDir(opts.name)
	if err != nil {
		return nil, err
	}

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clear stale authenticator staging directory %q: %w", dir, err)
	}

	staged, err := authn.Stage(dir, ca)
	if err != nil {
		return nil, fmt.Errorf("stage authenticator: %w", err)
	}

	return staged, nil
}

// authnStagingDir returns the host directory the authenticator's webhook
// kubeconfig and TLS certificate are staged under for the cluster named name,
// scoped by cluster name so concurrent `fjord create cluster` runs for
// different clusters do not collide.
func authnStagingDir(name string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}

	return filepath.Join(cacheDir, "fjord", "authn", name), nil
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
// giving it EKS-compatible AWS authentication. ca is the CA the
// authenticator's static pod manifest was already staged to trust (see
// stageAuthenticator); it is reused here for IRSA/pod-identity-webhook's
// serving certificates.
func deployAgent(ctx context.Context, logger log.Logger, provider kind.Provider, client kubernetes.Interface, opts *createClusterOptions, ca *pki.CA) error {
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

	logger.V(0).Infof("Deploying fjord-agent (%s) ...", image)

	if err := cluster.EnsureAgent(ctx, client, image, true); err != nil {
		return fmt.Errorf("ensure agent: %w", err)
	}

	if err := deployAuthenticator(ctx, logger, provider, client, image, opts.name); err != nil {
		return err
	}

	logger.V(0).Info("Deploying IRSA support (pod-identity-webhook + fjord injector) ...")

	if err := cluster.EnsureIRSA(ctx, client, ca, cluster.DefaultPodIdentityWebhookImage); err != nil {
		return fmt.Errorf("ensure irsa: %w", err)
	}

	if err := ensureNodeRolePrincipal(ctx, client, opts.nodeRoleName); err != nil {
		return err
	}

	logger.V(0).Info("Deploying IMDS emulation (fjord-imds) ...")

	if err := cluster.EnsureIMDS(ctx, client, image, opts.nodeRoleName); err != nil {
		return fmt.Errorf("ensure imds: %w", err)
	}

	logger.V(0).Info("Deploying EKS Pod Identity emulation (eks-pod-identity-agent) ...")

	if err := cluster.EnsurePodIdentity(ctx, client, cluster.DefaultPodIdentityAgentImage, opts.name); err != nil {
		return fmt.Errorf("ensure pod identity: %w", err)
	}

	return nil
}

// deployAuthenticator sets up fjord's authentication token webhook: its RBAC,
// the DaemonSet that serves it, and the cluster-info the EKS API facade needs.
func deployAuthenticator(ctx context.Context, logger log.Logger, provider kind.Provider, client kubernetes.Interface, image, name string) error {
	logger.V(0).Info("Deploying the authentication token webhook (fjord-authenticator) ...")

	if err := cluster.EnsureAuthenticatorRBAC(ctx, client); err != nil {
		return fmt.Errorf("ensure authenticator rbac: %w", err)
	}

	if err := cluster.EnsureAuthenticator(ctx, client, image); err != nil {
		return fmt.Errorf("ensure authenticator: %w", err)
	}

	return ensureClusterInfo(ctx, provider, client, name)
}

// ensureClusterInfo registers name's API server endpoint and certificate
// authority (read from kind's own kubeconfig for the cluster) in
// fjord-agent's ClusterInfo store, so the EKS API facade's DescribeCluster
// endpoint (which `fjord update-kubeconfig` calls) can serve them.
func ensureClusterInfo(ctx context.Context, provider kind.Provider, client kubernetes.Interface, name string) error {
	kubeconfig, err := provider.KubeConfig(name)
	if err != nil {
		return fmt.Errorf("get kubeconfig: %w", err)
	}

	config, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return fmt.Errorf("parse kubeconfig: %w", err)
	}

	kubeContext, ok := config.Contexts[config.CurrentContext]
	if !ok {
		return fmt.Errorf("kubeconfig has no context %q", config.CurrentContext)
	}

	kubeCluster, ok := config.Clusters[kubeContext.Cluster]
	if !ok {
		return fmt.Errorf("kubeconfig has no cluster %q", kubeContext.Cluster)
	}

	info := agent.ClusterInfo{
		Endpoint:                 kubeCluster.Server,
		CertificateAuthorityData: base64.StdEncoding.EncodeToString(kubeCluster.CertificateAuthorityData),
	}

	if err := agent.NewConfigMapClusterInfoStore(client).Put(ctx, info); err != nil {
		return fmt.Errorf("register cluster info: %w", err)
	}

	return nil
}

// ensureNodeRolePrincipal registers nodeRoleName's IAM role identity in the
// principal registry if it is not already there, so `fjord list principal`
// reflects the node role fjord-imds advertises to unannotated pods even
// before any pod has fetched credentials from it. fjord-imds keeps this
// principal's access key current as it (re)issues node role credentials.
func ensureNodeRolePrincipal(ctx context.Context, client kubernetes.Interface, nodeRoleName string) error {
	store := agent.NewSecretPrincipalStore(client)

	_, err := store.GetByName(ctx, nodeRoleName)
	if err == nil {
		return nil
	}

	if !errors.Is(err, agent.ErrPrincipalNotFound) {
		return fmt.Errorf("look up node role principal: %w", err)
	}

	accessKeyID, err := agent.GenerateAccessKeyID()
	if err != nil {
		return fmt.Errorf("generate node role access key id: %w", err)
	}

	principal := agent.Principal{
		AccessKeyID: accessKeyID,
		ARN:         agent.RoleARN(nodeRoleName),
		Name:        nodeRoleName,
	}

	if err := store.Put(ctx, principal); err != nil {
		return fmt.Errorf("register node role principal: %w", err)
	}

	return nil
}
