package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/authn"
	"github.com/sivchari/fjord/internal/cluster"
	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/kind"
	"github.com/sivchari/fjord/internal/logger"
	"github.com/sivchari/fjord/internal/nodeimage"
	"github.com/sivchari/fjord/internal/pki"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

const (
	defaultClusterName = "fjord"
	// defaultAgentHostPort is the default host port fjord-agent's fake STS
	// API is published on.
	defaultAgentHostPort = 48080
)

func newCreateCmd(logger logger.Logger) *cobra.Command {
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
	eksVersion       string
	name             string
	buildLocal       bool
	wait             time.Duration
	enableAuth       bool
	agentImage       string
	hostPort         int32
	nodeRoleName     string
	withKumo         bool
	kumoImage        string
	awsEndpointURL   string
	withLoadBalancer bool
}

func newCreateClusterCmd(logger logger.Logger) *cobra.Command {
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
	cmd.Flags().BoolVar(&opts.withKumo, "with-kumo", true,
		"deploy kumo (a local AWS emulator) and point IAM-identity pods' AWS SDK calls at it (use --with-kumo=false to disable)")
	cmd.Flags().StringVar(&opts.kumoImage, "kumo-image", cluster.DefaultKumoImage, "kumo image to deploy (with --with-kumo)")
	cmd.Flags().StringVar(&opts.awsEndpointURL, "aws-endpoint-url", "",
		"AWS_ENDPOINT_URL value to inject into IAM-identity pods for non-STS AWS calls, overriding --with-kumo's own endpoint (e.g. to point at an external kumo/LocalStack instance)")
	cmd.Flags().BoolVar(&opts.withLoadBalancer, "with-loadbalancer", false,
		"deploy metallb so type: LoadBalancer Services get an external IP from the kind docker network")

	return cmd
}

func runCreateCluster(ctx context.Context, logger logger.Logger, opts *createClusterOptions) error {
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

	err = provider.CreateCluster(opts.name, clusterprovider.CreateOptions{
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

	if err := applyEKSDefaultState(ctx, logger, provider, client, opts.name); err != nil {
		return err
	}

	if opts.enableAuth {
		if err := deployAgent(ctx, logger, provider, client, opts, ca, release.EKSVersion); err != nil {
			return err
		}
	}

	if opts.withLoadBalancer {
		if err := deployLoadBalancer(ctx, logger, provider, opts.name); err != nil {
			return err
		}
	}

	logger.V(0).Infof("Cluster %q is ready. Set kubectl context to \"kind-%s\".", opts.name, opts.name)

	return nil
}

// applyEKSDefaultState brings a freshly created cluster to the default state
// a new Amazon EKS cluster starts in: the gp2/gp3 StorageClasses, and the
// ClusterNetworkPolicy (networking.k8s.aws/v1alpha1) and TargetGroupBinding
// (elbv2.k8s.aws/v1beta1) CRDs registered so a real cluster's manifests for
// either apply unmodified. Unlike --with-loadbalancer or --enable-auth, all
// of these always run: fjord emulates them unconditionally, matching what
// every real EKS cluster already has.
func applyEKSDefaultState(ctx context.Context, logger logger.Logger, provider clusterprovider.Provider, client kubernetes.Interface, name string) error {
	logger.V(0).Info("Applying EKS default state (gp2 StorageClass) ...")

	if err := cluster.EnsureDefaultStorageClass(ctx, client); err != nil {
		return fmt.Errorf("ensure default storage class: %w", err)
	}

	dynamicClient, err := clusterDynamicClient(provider, name)
	if err != nil {
		return err
	}

	logger.V(0).Info("Registering the ClusterNetworkPolicy CRD (networking.k8s.aws/v1alpha1) ...")

	if err := cluster.EnsureClusterNetworkPolicyCRD(ctx, client, dynamicClient); err != nil {
		return fmt.Errorf("ensure cluster network policy crd: %w", err)
	}

	logger.V(0).Info("Registering the TargetGroupBinding CRD (elbv2.k8s.aws/v1beta1) ...")

	if err := cluster.EnsureTargetGroupBindingCRD(ctx, client, dynamicClient); err != nil {
		return fmt.Errorf("ensure target group binding crd: %w", err)
	}

	return nil
}

// deployLoadBalancer deploys metallb so type: LoadBalancer Services get an
// external IP, using an address range carved out of the cluster's kind docker
// network subnet.
func deployLoadBalancer(ctx context.Context, logger logger.Logger, provider clusterprovider.Provider, name string) error {
	subnet, err := dockerNetworkSubnet(ctx)
	if err != nil {
		return err
	}

	addressRange, err := metalLBAddressRange(subnet)
	if err != nil {
		return err
	}

	dynamicClient, err := clusterDynamicClient(provider, name)
	if err != nil {
		return err
	}

	client, err := clusterClient(provider, name)
	if err != nil {
		return err
	}

	logger.V(0).Infof("Deploying metallb (LoadBalancer address range %s) ...", addressRange)

	if err := cluster.EnsureLoadBalancer(ctx, client, dynamicClient, addressRange); err != nil {
		return fmt.Errorf("ensure load balancer: %w", err)
	}

	return nil
}

// buildKindConfig builds the clusterprovider.Config for opts and release.
// When auth is enabled it also generates the fjord CA and stages the
// authenticator's static pod manifest, webhook kubeconfig, and TLS
// certificate (see stageAuthenticator), wiring the result into the returned
// Config's ExtraMounts and AuthWebhook: both must exist before the cluster
// is created, since kind delivers them into the control-plane node as part
// of cluster creation itself, before the API server ever starts. The
// returned CA is nil when auth is disabled; otherwise it is reused later for
// IRSA/pod-identity-webhook's serving certificates, so the webhook
// kubeconfig's trust and every fjord-issued certificate share one root.
func buildKindConfig(opts *createClusterOptions, release *eksd.Release) (*clusterprovider.Config, *pki.CA, error) {
	coreDNSRepo, coreDNSTag := coreDNSKubeadmRepository(release.CoreDNSImage)

	config := &clusterprovider.Config{
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
func resolveNodeImage(ctx context.Context, logger logger.Logger, opts *createClusterOptions, release *eksd.Release) (string, error) {
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
func clusterClient(provider clusterprovider.Provider, name string) (kubernetes.Interface, error) {
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

// clusterDynamicClient builds a dynamic client for the cluster named name,
// used to apply metallb's native manifest and custom resources.
func clusterDynamicClient(provider clusterprovider.Provider, name string) (dynamic.Interface, error) {
	kubeconfig, err := provider.KubeConfig(name)
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig: %w", err)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	return dynamicClient, nil
}

// dockerNetworkName is the docker network kind attaches its node containers
// to.
const dockerNetworkName = "kind"

// dockerNetworkSubnet returns the IPv4 subnet of the kind docker network,
// read via `docker network inspect`. The network carries both an IPv6 and an
// IPv4 subnet; ipv4Subnet picks the IPv4 one.
func dockerNetworkSubnet(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", dockerNetworkName,
		"--format", "{{range .IPAM.Config}}{{.Subnet}} {{end}}").Output()
	if err != nil {
		return "", fmt.Errorf("inspect %q docker network: %w", dockerNetworkName, err)
	}

	subnet, err := ipv4Subnet(strings.TrimSpace(string(out)))
	if err != nil {
		return "", fmt.Errorf("resolve %q docker network subnet: %w", dockerNetworkName, err)
	}

	return subnet, nil
}

// deployAgent builds (if requested) and deploys fjord-agent to the cluster,
// giving it EKS-compatible AWS authentication. ca is the CA the
// authenticator's static pod manifest was already staged to trust (see
// stageAuthenticator); it is reused here for IRSA/pod-identity-webhook's
// serving certificates. eksVersion is registered in the EKS API facade's
// ClusterInfo store so DescribeCluster/ListClusters can report it.
func deployAgent(ctx context.Context, logger logger.Logger, provider clusterprovider.Provider, client kubernetes.Interface, opts *createClusterOptions, ca *pki.CA, eksVersion string) error {
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

	if opts.withKumo {
		logger.V(0).Infof("Deploying kumo (%s) ...", opts.kumoImage)

		if err := cluster.EnsureKumo(ctx, client, opts.kumoImage); err != nil {
			return fmt.Errorf("ensure kumo: %w", err)
		}
	}

	logger.V(0).Infof("Deploying fjord-agent (%s) ...", image)

	if err := cluster.EnsureAgent(ctx, client, image, true, resolveAWSEndpointURL(opts)); err != nil {
		return fmt.Errorf("ensure agent: %w", err)
	}

	if err := deployAuthenticator(ctx, logger, provider, client, image, opts.name, eksVersion); err != nil {
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

// resolveAWSEndpointURL returns the AWS_ENDPOINT_URL value to inject into
// IAM-identity pods: opts.awsEndpointURL if explicitly set (taking
// precedence, so callers can point at an external kumo/LocalStack instance),
// else cluster.KumoEndpoint if opts.withKumo requested fjord's own kumo
// deployment, else "" (no injection, preserving prior behavior).
func resolveAWSEndpointURL(opts *createClusterOptions) string {
	if opts.awsEndpointURL != "" {
		return opts.awsEndpointURL
	}

	if opts.withKumo {
		return cluster.KumoEndpoint
	}

	return ""
}

// deployAuthenticator sets up fjord's authentication token webhook: its RBAC,
// the DaemonSet that serves it, and the cluster-info the EKS API facade needs.
func deployAuthenticator(ctx context.Context, logger logger.Logger, provider clusterprovider.Provider, client kubernetes.Interface, image, name, eksVersion string) error {
	logger.V(0).Info("Deploying the authentication token webhook (fjord-authenticator) ...")

	if err := cluster.EnsureAuthenticatorRBAC(ctx, client); err != nil {
		return fmt.Errorf("ensure authenticator rbac: %w", err)
	}

	if err := cluster.EnsureAuthenticator(ctx, client, image); err != nil {
		return fmt.Errorf("ensure authenticator: %w", err)
	}

	return ensureClusterInfo(ctx, provider, client, name, eksVersion)
}

// ensureClusterInfo registers name's API server endpoint, certificate
// authority (read from kind's own kubeconfig for the cluster), and EKS
// version in fjord-agent's ClusterInfo store, so the EKS API facade's
// DescribeCluster and ListClusters endpoints (which `fjord update-kubeconfig`
// and standard EKS tooling call) can serve them.
func ensureClusterInfo(ctx context.Context, provider clusterprovider.Provider, client kubernetes.Interface, name, eksVersion string) error {
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
		Name:                     name,
		Endpoint:                 kubeCluster.Server,
		CertificateAuthorityData: base64.StdEncoding.EncodeToString(kubeCluster.CertificateAuthorityData),
		Version:                  eksVersion,
	}

	if err := agent.NewConfigMapClusterInfoStore(client).Put(ctx, &info); err != nil {
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
