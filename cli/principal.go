package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sivchari/fjord/internal/agent"
	"github.com/sivchari/fjord/internal/logger"
)

// principalRegistryOptions carries the cluster/context flags shared by
// every `fjord * principal` subcommand.
type principalRegistryOptions struct {
	clusterName string
	kubeContext string
}

// registerFlags attaches the shared --name/--context flags to cmd.
func (o *principalRegistryOptions) registerFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.clusterName, "name", defaultClusterName, "cluster name")
	cmd.Flags().StringVar(&o.kubeContext, "context", "", "kubeconfig context to use (default: kind-<name>)")
}

// context resolves o.kubeContext if set, otherwise the kind context for
// o.clusterName.
func (o *principalRegistryOptions) context() string {
	if o.kubeContext != "" {
		return o.kubeContext
	}

	return "kind-" + o.clusterName
}

// client builds a Kubernetes client from the default kubeconfig, targeting
// o.context().
func (o *principalRegistryOptions) client() (kubernetes.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.context()}

	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}

	return client, nil
}

func newCreatePrincipalCmd(logger logger.Logger) *cobra.Command {
	opts := &principalRegistryOptions{}

	cmd := &cobra.Command{
		Use:   "principal <name>",
		Short: "Create an IAM principal and register its access key in the fjord-principals registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCreatePrincipal(cmd, logger, opts, args[0])
		},
	}

	opts.registerFlags(cmd)

	return cmd
}

func runCreatePrincipal(cmd *cobra.Command, logger logger.Logger, opts *principalRegistryOptions, name string) error {
	client, err := opts.client()
	if err != nil {
		return err
	}

	accessKeyID, err := agent.GenerateAccessKeyID()
	if err != nil {
		return fmt.Errorf("generate access key id: %w", err)
	}

	principal := agent.Principal{
		AccessKeyID: accessKeyID,
		ARN:         agent.PrincipalARN(name),
		Name:        name,
	}

	logger.V(0).Infof("Registering principal %q ...", name)

	if err := agent.NewSecretPrincipalStore(client).Put(cmd.Context(), principal); err != nil {
		return fmt.Errorf("register principal: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"access-key-id: %s\narn: %s\n\nUse this access key id when configuring credentials for this principal (e.g. via \"fjord update-kubeconfig\").\n",
		principal.AccessKeyID, principal.ARN)

	return nil
}

func newListCmd(logger logger.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List resources",
	}
	cmd.AddCommand(newListPrincipalCmd(logger))

	return cmd
}

func newListPrincipalCmd(_ logger.Logger) *cobra.Command {
	opts := &principalRegistryOptions{}

	cmd := &cobra.Command{
		Use:   "principal",
		Short: "List registered IAM principals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runListPrincipal(cmd, opts)
		},
	}

	opts.registerFlags(cmd)

	return cmd
}

func runListPrincipal(cmd *cobra.Command, opts *principalRegistryOptions) error {
	client, err := opts.client()
	if err != nil {
		return err
	}

	principals, err := agent.NewSecretPrincipalStore(client).List(cmd.Context())
	if err != nil {
		return fmt.Errorf("list principals: %w", err)
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(tw, "NAME\tACCESS-KEY-ID\tARN")

	for _, p := range principals {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.AccessKeyID, p.ARN)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}

	return nil
}

func newDeletePrincipalCmd(logger logger.Logger) *cobra.Command {
	opts := &principalRegistryOptions{}

	cmd := &cobra.Command{
		Use:   "principal <name>",
		Short: "Delete an IAM principal from the fjord-principals registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeletePrincipal(cmd, logger, opts, args[0])
		},
	}

	opts.registerFlags(cmd)

	return cmd
}

func runDeletePrincipal(cmd *cobra.Command, logger logger.Logger, opts *principalRegistryOptions, name string) error {
	client, err := opts.client()
	if err != nil {
		return err
	}

	if err := agent.NewSecretPrincipalStore(client).Delete(cmd.Context(), name); err != nil {
		return fmt.Errorf("delete principal: %w", err)
	}

	logger.V(0).Infof("Deleted principal %q", name)

	return nil
}
