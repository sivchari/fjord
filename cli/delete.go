package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/kind"
)

func newDeleteCmd(logger log.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a resource",
	}
	cmd.AddCommand(newDeleteClusterCmd(logger))
	cmd.AddCommand(newDeletePrincipalCmd(logger))

	return cmd
}

func newDeleteClusterCmd(logger log.Logger) *cobra.Command {
	var name string

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Delete a cluster",
		RunE: func(_ *cobra.Command, _ []string) error {
			logger.V(0).Infof("Deleting cluster %q ...", name)

			provider := kind.NewProvider(logger)
			if err := provider.DeleteCluster(name, ""); err != nil {
				return fmt.Errorf("delete cluster: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", defaultClusterName, "cluster name")

	return cmd
}
