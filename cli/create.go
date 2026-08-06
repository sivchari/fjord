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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/authn"
	"github.com/sivchari/fjord/internal/cluster"
	"github.com/sivchari/fjord/internal/componentdir"
	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/logger"
	"github.com/sivchari/fjord/internal/pki"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

const defaultClusterName = "fjord"

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
	cmd.Flags().BoolVar(&opts.buildLocal, "build-local", false, "build and load the fjord-agent image locally instead of pulling the published one")
	cmd.Flags().DurationVar(&opts.wait, "wait", 2*time.Minute, "wait for the control plane to be ready (0 to disable)")
	cmd.Flags().BoolVar(&opts.enableAuth, "enable-auth", true, "deploy fjord-agent for EKS-compatible AWS authentication")
	cmd.Flags().StringVar(&opts.agentImage, "agent-image", "", "fjord-agent image to deploy (default: "+agentImageRepository+":<version>, or a locally built image with --build-local)")
	cmd.Flags().StringVar(&opts.nodeRoleName, "node-role-name", agent.DefaultNodeRoleName,
		"IAM role name fjord-imds advertises as the node's instance role")
	cmd.Flags().BoolVar(&opts.withKumo, "with-kumo", true,
		"deploy kumo (a local AWS emulator) and point IAM-identity pods' AWS SDK calls at it (use --with-kumo=false to disable)")
	cmd.Flags().StringVar(&opts.kumoImage, "kumo-image", cluster.DefaultKumoImage, "kumo image to deploy (with --with-kumo)")
	cmd.Flags().StringVar(&opts.awsEndpointURL, "aws-endpoint-url", "",
		"AWS_ENDPOINT_URL value to inject into IAM-identity pods for non-STS AWS calls, overriding --with-kumo's own endpoint (e.g. to point at an external kumo/LocalStack instance)")
	cmd.Flags().BoolVar(&opts.withLoadBalancer, "with-loadbalancer", false,
		"enable fjord-agent's LoadBalancer controller, which claims type: LoadBalancer Services with the cluster's node address(es) (requires --enable-auth)")

	return cmd
}

func runCreateCluster(ctx context.Context, logger logger.Logger, opts *createClusterOptions) error {
	if err := validateCreateClusterOptions(opts); err != nil {
		return err
	}

	release, err := resolveEKSRelease(opts)
	if err != nil {
		return err
	}

	config, ca, authenticatorCert, err := buildClusterConfig(opts, release)
	if err != nil {
		return err
	}

	logger.V(0).Infof("Creating cluster %q (EKS %s / %s) ...", opts.name, release.EKSVersion, release.KubeVersion)

	provider, err := newClusterProvider()
	if err != nil {
		return err
	}

	createOpts, err := buildCreateOptions(ctx, logger, opts, config, release)
	if err != nil {
		return err
	}

	if err := provider.CreateCluster(ctx, opts.name, createOpts); err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	if err := mergeReadyKubeconfig(logger, provider, opts.name); err != nil {
		return err
	}

	client, err := clusterClient(provider, opts.name)
	if err != nil {
		return err
	}

	if err := applyEKSDefaultState(ctx, logger, provider, client, opts.name); err != nil {
		return err
	}

	if opts.enableAuth {
		if err := deployAgent(ctx, logger, provider, client, opts, ca, authenticatorCert, release.EKSVersion); err != nil {
			return err
		}
	}

	logger.V(0).Info(clusterReadyMessage(opts))

	return nil
}

// resolveEKSRelease resolves the EKS Distro release opts asks for, defaulting
// to the latest supported EKS version when --eks-version is unset.
func resolveEKSRelease(opts *createClusterOptions) (*eksd.Release, error) {
	eksVersion := opts.eksVersion
	if eksVersion == "" {
		eksVersion = latestVersion(eksd.SupportedVersions())
	}

	release, err := eksd.Resolve(eksVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve eks version: %w", err)
	}

	return release, nil
}

// buildCreateOptions builds the clusterprovider.CreateOptions for opts,
// materializing a component directory from release's EKS-D server tarball
// for rask to run the cluster's nodes from, on every platform rask supports.
func buildCreateOptions(ctx context.Context, logger logger.Logger, opts *createClusterOptions, config *clusterprovider.Config, release *eksd.Release) (clusterprovider.CreateOptions, error) {
	createOpts := clusterprovider.CreateOptions{
		Config:       config,
		WaitForReady: opts.wait,
	}

	componentDir, err := componentdir.Materialize(ctx, release, buildArch())
	if err != nil {
		return clusterprovider.CreateOptions{}, fmt.Errorf("materialize eks-d components: %w", err)
	}

	logger.V(0).Infof("Using EKS-D components at %s ...", componentDir)

	createOpts.ComponentDir = componentDir

	return createOpts, nil
}

// clusterReadyMessage builds the final "cluster is ready" message for opts,
// naming the kubectl context runCreateCluster already merged and switched to
// (see mergeClusterKubeconfig) and, when auth is enabled, the endpoint
// fjord-agent's fake STS API is reachable at.
func clusterReadyMessage(opts *createClusterOptions) string {
	message := fmt.Sprintf("Cluster %q is ready. kubectl context is set to %q.", opts.name, opts.name)

	if opts.enableAuth {
		message += fmt.Sprintf(" fjord-agent's fake STS API is reachable at %s:%d.", cluster.AgentLoopbackHost, cluster.AgentNodePort)
	}

	return message
}

// validateCreateClusterOptions rejects create cluster requests fjord cannot
// satisfy.
func validateCreateClusterOptions(opts *createClusterOptions) error {
	if opts.withLoadBalancer && !opts.enableAuth {
		return errors.New("--with-loadbalancer requires --enable-auth: fjord's LoadBalancer controller runs inside fjord-agent, which is only deployed when authentication is enabled (decoupling agent deployment from --enable-auth is follow-up work)")
	}

	return nil
}

// applyEKSDefaultState brings a freshly created cluster to the default state
// a new Amazon EKS cluster starts in: the gp2/gp3 StorageClasses, the
// ClusterNetworkPolicy (networking.k8s.aws/v1alpha1) and TargetGroupBinding
// (elbv2.k8s.aws/v1beta1) CRDs registered so a real cluster's manifests for
// either apply unmodified, and the kube-network-policies enforcer deployed
// so NetworkPolicy objects actually take effect (rask's bridge CNI enforces
// nothing; real EKS enforces via the VPC CNI's network policy agent).
// Unlike --with-loadbalancer or --enable-auth, all of these always run:
// fjord emulates them unconditionally, matching what every real EKS cluster
// already has.
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

	logger.V(0).Info("Deploying the NetworkPolicy enforcer (kube-network-policies) ...")

	if err := cluster.EnsureNetworkPolicyEnforcer(ctx, client, dynamicClient); err != nil {
		return fmt.Errorf("ensure network policy enforcer: %w", err)
	}

	return nil
}

// buildClusterConfig builds the clusterprovider.Config for opts and release.
// When auth is enabled it also generates the fjord CA and stages the
// authenticator's webhook kubeconfig and TLS certificate (see
// stageAuthenticator), wiring the webhook kubeconfig into the returned
// Config's ExtraMounts and AuthWebhook: both must exist before the cluster
// is created, since rask delivers them into the control-plane node as part
// of cluster creation itself, before the API server ever starts. The
// returned CA and authenticator certificate are nil when auth is disabled;
// otherwise the CA is reused later for IRSA/pod-identity-webhook's serving
// certificates, so the webhook kubeconfig's trust and every fjord-issued
// certificate share one root, and the authenticator certificate is reused by
// deployAgent/deployAuthenticator to populate the Secret
// cluster.EnsureAuthenticator's DaemonSet mounts, so the pod serves the same
// certificate the webhook kubeconfig's SANs and trust were computed against.
func buildClusterConfig(opts *createClusterOptions, release *eksd.Release) (*clusterprovider.Config, *pki.CA, *pki.ServerCert, error) {
	config := &clusterprovider.Config{
		Name:        opts.name,
		KubeVersion: release.KubeVersion,
	}

	// The full repository path is passed through: rask joins repository:tag
	// verbatim, unlike kubeadm, which appended the image name to a parent
	// path itself.
	config.CoreDNSImageRepository, config.CoreDNSImageTag = splitImageRef(release.CoreDNSImage)

	if !opts.enableAuth {
		return config, nil, nil, nil
	}

	ca, err := pki.NewCA("fjord")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate fjord CA: %w", err)
	}

	staged, err := stageAuthenticator(opts, ca)
	if err != nil {
		return nil, nil, nil, err
	}

	config.ExtraMounts = staged.Mounts
	config.AuthWebhook = staged.Webhook

	return config, ca, staged.Cert, nil
}

// stageAuthenticator stages the authenticator's webhook kubeconfig under a
// per-cluster host directory (see authnStagingDir) and issues its TLS
// certificate from ca, clearing any stale staging left by a previous cluster
// of the same name first.
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

// mergeReadyKubeconfig merges name's kubeconfig into the user's default
// kubeconfig (see mergeClusterKubeconfig) once the cluster is up, so
// kubectl can reach it under context name without any further setup, and
// logs where it was written.
func mergeReadyKubeconfig(logger logger.Logger, provider clusterprovider.Provider, name string) error {
	path, err := mergeClusterKubeconfig(provider, name)
	if err != nil {
		return fmt.Errorf("merge kubeconfig: %w", err)
	}

	logger.V(0).Infof("Merged kubeconfig for cluster %q into %s", name, path)

	return nil
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
// used to apply the ClusterNetworkPolicy and TargetGroupBinding CRDs.
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

// deployAgent builds (if requested) and deploys fjord-agent to the cluster,
// giving it EKS-compatible AWS authentication. ca is the CA the
// authenticator's webhook kubeconfig was already staged to trust (see
// stageAuthenticator); it is reused here for IRSA/pod-identity-webhook's
// serving certificates. authenticatorCert is the authenticator's own TLS
// certificate, issued from ca by the same staging call; deployAuthenticator
// passes it through to cluster.EnsureAuthenticator to populate the Secret
// its DaemonSet mounts. eksVersion is registered in the EKS API facade's
// ClusterInfo store so DescribeCluster/ListClusters can report it.
func deployAgent(ctx context.Context, logger logger.Logger, provider clusterprovider.Provider, client kubernetes.Interface, opts *createClusterOptions, ca *pki.CA, authenticatorCert *pki.ServerCert, eksVersion string) error {
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

		if err := provider.LoadDockerImage(ctx, opts.name, image); err != nil {
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

	if err := cluster.EnsureAgent(ctx, client, image, true, resolveAWSEndpointURL(opts), opts.withLoadBalancer); err != nil {
		return fmt.Errorf("ensure agent: %w", err)
	}

	if err := deployAuthenticator(ctx, logger, provider, client, image, opts.name, eksVersion, authenticatorCert); err != nil {
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
// the DaemonSet that serves it (its Secret populated from cert, the
// authenticator's TLS certificate staged by stageAuthenticator), and the
// cluster-info the EKS API facade needs.
func deployAuthenticator(ctx context.Context, logger logger.Logger, provider clusterprovider.Provider, client kubernetes.Interface, image, name, eksVersion string, cert *pki.ServerCert) error {
	logger.V(0).Info("Deploying the authentication token webhook (fjord-authenticator) ...")

	if err := cluster.EnsureAuthenticatorRBAC(ctx, client); err != nil {
		return fmt.Errorf("ensure authenticator rbac: %w", err)
	}

	if err := cluster.EnsureAuthenticator(ctx, client, image, cert); err != nil {
		return fmt.Errorf("ensure authenticator: %w", err)
	}

	return ensureClusterInfo(ctx, provider, client, name, eksVersion)
}

// ensureClusterInfo registers name's API server endpoint, certificate
// authority (read from the provider's own kubeconfig for the cluster), and
// EKS version in fjord-agent's ClusterInfo store, so the EKS API facade's
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
