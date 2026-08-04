package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/cluster"
	"github.com/sivchari/fjord/internal/logger"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

const (
	// execAWSRegion is the --region every generated exec credential plugin
	// passes to `aws eks get-token`, matching every other dummy region
	// value fjord reports (see internal/agent/imds.go's imdsRegion).
	execAWSRegion = "us-east-1"

	// execAWSSignValue is the AWS_SECRET_ACCESS_KEY value every generated
	// exec credential entry sets. fjord ignores SigV4 signatures entirely
	// (see internal/agent/sts.go's resolveAccessKeyID), so only
	// AWS_ACCESS_KEY_ID needs to carry the principal's real access key id;
	// this value only needs to be non-empty for the AWS CLI's credential
	// chain to consider the static credentials configured, matching
	// internal/cluster/podidentity.go's podIdentityAgentSignValue.
	execAWSSignValue = "fjord-dummy-signing-value"
)

// updateKubeconfigOptions carries `fjord update-kubeconfig`'s flag values.
type updateKubeconfigOptions struct {
	principalRegistryOptions
	principal string
}

func newUpdateKubeconfigCmd(logger logger.Logger) *cobra.Command {
	opts := &updateKubeconfigOptions{}

	cmd := &cobra.Command{
		Use:   "update-kubeconfig",
		Short: "Add a principal's EKS-compatible exec credential context to the default kubeconfig",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdateKubeconfig(cmd, logger, opts)
		},
	}

	opts.registerFlags(cmd)
	cmd.Flags().StringVar(&opts.principal, "principal", "", "principal name registered via \"fjord create principal\"")

	_ = cmd.MarkFlagRequired("principal")

	return cmd
}

func runUpdateKubeconfig(cmd *cobra.Command, logger logger.Logger, opts *updateKubeconfigOptions) error {
	client, err := opts.client()
	if err != nil {
		return err
	}

	principal, err := agent.NewSecretPrincipalStore(client).GetByName(cmd.Context(), opts.principal)
	if err != nil {
		return fmt.Errorf("resolve principal %q: %w", opts.principal, err)
	}

	sourceCluster, err := loadClusterConfig(opts.context())
	if err != nil {
		return err
	}

	path, contextName, err := writeKubeconfigEntry(opts, principal, sourceCluster)
	if err != nil {
		return err
	}

	logger.V(0).Infof("Updated %s: added context %q for principal %q", path, contextName, opts.principal)

	return nil
}

// loadClusterConfig returns the Cluster (server URL and certificate
// authority) rask wrote for kubeContext in the default kubeconfig, the same
// entry `fjord create cluster` left behind.
func loadClusterConfig(kubeContext string) (*clientcmdapi.Cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	entryContext, ok := config.Contexts[kubeContext]
	if !ok {
		return nil, fmt.Errorf("kubeconfig has no context %q", kubeContext)
	}

	entryCluster, ok := config.Clusters[entryContext.Cluster]
	if !ok {
		return nil, fmt.Errorf("kubeconfig has no cluster %q", entryContext.Cluster)
	}

	return entryCluster, nil
}

// writeKubeconfigEntry adds a Cluster/AuthInfo/Context triple for
// principal to the default kubeconfig file, pointing kubectl at
// sourceCluster's server/CA and authenticating via `aws eks get-token`. It
// returns the kubeconfig file path and the name of the context it added
// (and made current), matching the shape `aws eks update-kubeconfig`
// leaves behind.
func writeKubeconfigEntry(opts *updateKubeconfigOptions, principal *agent.Principal, sourceCluster *clientcmdapi.Cluster) (path, contextName string, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	path = rules.GetDefaultFilename()

	config, err := loadOrNewKubeconfig(path)
	if err != nil {
		return "", "", err
	}

	entryName := fmt.Sprintf("fjord-%s@%s", opts.principal, opts.clusterName)

	config.Clusters[entryName] = sourceCluster
	config.AuthInfos[entryName] = execAuthInfo(opts, principal)
	config.Contexts[entryName] = &clientcmdapi.Context{Cluster: entryName, AuthInfo: entryName}
	config.CurrentContext = entryName

	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return "", "", fmt.Errorf("write kubeconfig: %w", err)
	}

	return path, entryName, nil
}

// loadOrNewKubeconfig loads the kubeconfig at path, or returns a fresh
// empty Config if it does not exist yet.
func loadOrNewKubeconfig(path string) (*clientcmdapi.Config, error) {
	config, err := clientcmd.LoadFromFile(path)
	if err == nil {
		return config, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("load kubeconfig %q: %w", path, err)
	}

	return clientcmdapi.NewConfig(), nil
}

// mergeClusterKubeconfig merges the Cluster/AuthInfo/Context provider wrote
// for the cluster named name (see provider.KubeConfig) into the user's
// default kubeconfig file, all keyed under name, and sets current-context to
// name — the same shape `kind` used to leave behind on cluster creation. The
// source AuthInfo entry is renamed to name even though rask's per-cluster
// kubeconfig always calls it "admin" (see internal/rask/provider.go's
// KubeConfig doc), since keeping that name would collide across multiple
// clusters merged into the same default kubeconfig. It returns the default
// kubeconfig's path.
func mergeClusterKubeconfig(provider clusterprovider.Provider, name string) (path string, err error) {
	raw, err := provider.KubeConfig(name)
	if err != nil {
		return "", fmt.Errorf("get kubeconfig: %w", err)
	}

	source, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}

	sourceContext, ok := source.Contexts[name]
	if !ok {
		return "", fmt.Errorf("cluster kubeconfig has no context %q", name)
	}

	sourceCluster, ok := source.Clusters[sourceContext.Cluster]
	if !ok {
		return "", fmt.Errorf("cluster kubeconfig has no cluster %q", sourceContext.Cluster)
	}

	sourceAuthInfo, ok := source.AuthInfos[sourceContext.AuthInfo]
	if !ok {
		return "", fmt.Errorf("cluster kubeconfig has no authinfo %q", sourceContext.AuthInfo)
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	path = rules.GetDefaultFilename()

	config, err := loadOrNewKubeconfig(path)
	if err != nil {
		return "", err
	}

	config.Clusters[name] = sourceCluster
	config.AuthInfos[name] = sourceAuthInfo
	config.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	config.CurrentContext = name

	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return "", fmt.Errorf("write kubeconfig: %w", err)
	}

	return path, nil
}

// removeClusterKubeconfig removes the Cluster/AuthInfo/Context entries
// mergeClusterKubeconfig added for the cluster named name from the user's
// default kubeconfig file, clearing current-context if it pointed at name.
// It is a no-op, not an error, if the default kubeconfig file does not exist
// or name was never merged into it.
func removeClusterKubeconfig(name string) error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	path := rules.GetDefaultFilename()

	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("load kubeconfig %q: %w", path, err)
	}

	_, hasCluster := config.Clusters[name]
	_, hasAuthInfo := config.AuthInfos[name]
	_, hasContext := config.Contexts[name]

	if !hasCluster && !hasAuthInfo && !hasContext {
		return nil
	}

	delete(config.Clusters, name)
	delete(config.AuthInfos, name)
	delete(config.Contexts, name)

	if config.CurrentContext == name {
		config.CurrentContext = ""
	}

	if err := clientcmd.WriteToFile(*config, path); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	return nil
}

// execAuthInfo builds the AuthInfo entry for principal: an exec credential
// plugin invoking `aws eks get-token`, pointed at fjord-agent's fake STS
// (via AWS_ENDPOINT_URL_STS) and configured with principal's access key id.
func execAuthInfo(opts *updateKubeconfigOptions, principal *agent.Principal) *clientcmdapi.AuthInfo {
	return &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion: "client.authentication.k8s.io/v1beta1",
			Command:    "aws",
			Args:       []string{"--region", execAWSRegion, "eks", "get-token", "--cluster-name", opts.clusterName},
			Env: []clientcmdapi.ExecEnvVar{
				{Name: "AWS_ENDPOINT_URL_STS", Value: fmt.Sprintf("http://%s:%d", cluster.AgentLoopbackHost, cluster.AgentNodePort)},
				{Name: "AWS_ACCESS_KEY_ID", Value: principal.AccessKeyID},
				{Name: "AWS_SECRET_ACCESS_KEY", Value: execAWSSignValue},
			},
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
		},
	}
}
