package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/agent"
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
	hostPort  int32
}

func newUpdateKubeconfigCmd(logger log.Logger) *cobra.Command {
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
	cmd.Flags().Int32Var(&opts.hostPort, "agent-host-port", defaultAgentHostPort, "host port fjord-agent's fake STS API is published on")

	_ = cmd.MarkFlagRequired("principal")

	return cmd
}

func runUpdateKubeconfig(cmd *cobra.Command, logger log.Logger, opts *updateKubeconfigOptions) error {
	client, err := opts.client()
	if err != nil {
		return err
	}

	principal, err := agent.NewSecretPrincipalStore(client).GetByName(cmd.Context(), opts.principal)
	if err != nil {
		return fmt.Errorf("resolve principal %q: %w", opts.principal, err)
	}

	sourceCluster, err := loadKindClusterConfig(opts.context())
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

// loadKindClusterConfig returns the Cluster (server URL and certificate
// authority) kind wrote for kubeContext in the default kubeconfig, the same
// entry `fjord create cluster` left behind.
func loadKindClusterConfig(kubeContext string) (*clientcmdapi.Cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	kindContext, ok := config.Contexts[kubeContext]
	if !ok {
		return nil, fmt.Errorf("kubeconfig has no context %q", kubeContext)
	}

	kindCluster, ok := config.Clusters[kindContext.Cluster]
	if !ok {
		return nil, fmt.Errorf("kubeconfig has no cluster %q", kindContext.Cluster)
	}

	return kindCluster, nil
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
				{Name: "AWS_ENDPOINT_URL_STS", Value: fmt.Sprintf("http://localhost:%d", opts.hostPort)},
				{Name: "AWS_ACCESS_KEY_ID", Value: principal.AccessKeyID},
				{Name: "AWS_SECRET_ACCESS_KEY", Value: execAWSSignValue},
			},
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
		},
	}
}
