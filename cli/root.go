// Package cli implements the fjord command line interface.
package cli

import (
	"os"

	"github.com/spf13/cobra"

	fjord "github.com/sivchari/fjord"
	"github.com/sivchari/fjord/internal/logger"
)

// NewRootCmd returns the root fjord command.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "fjord",
		Short:         "fjord runs EKS-compatible Kubernetes clusters locally",
		Long:          "fjord creates local Kubernetes clusters that behave like Amazon EKS from the inside, built from EKS Distro.",
		Version:       fjord.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	logger := logger.NewStderr(os.Stderr, 0)

	cmd.AddCommand(newCreateCmd(logger))
	cmd.AddCommand(newDeleteCmd(logger))
	cmd.AddCommand(newBuildCmd(logger))
	cmd.AddCommand(newListCmd(logger))
	cmd.AddCommand(newGrantCmd(logger))
	cmd.AddCommand(newUpdateKubeconfigCmd(logger))

	return cmd
}
