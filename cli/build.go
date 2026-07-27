package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/nodeimage"
)

func newBuildCmd(logger log.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build an artifact",
	}
	cmd.AddCommand(newBuildNodeImageCmd(logger))

	return cmd
}

func newBuildNodeImageCmd(logger log.Logger) *cobra.Command {
	var (
		eksVersion   string
		arch         string
		listVersions bool
	)

	cmd := &cobra.Command{
		Use:   "node-image",
		Short: "Build a node image from EKS Distro artifacts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if listVersions {
				for _, v := range eksd.SupportedVersions() {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), v)
				}

				return nil
			}

			if eksVersion == "" {
				eksVersion = latestVersion(eksd.SupportedVersions())
			}

			release, err := eksd.Resolve(eksVersion)
			if err != nil {
				return fmt.Errorf("resolve eks version: %w", err)
			}

			logger.V(0).Infof("Building node image for EKS %s (%s, %s) ...", release.EKSVersion, release.KubeVersion, arch)

			image, err := nodeimage.Build(cmd.Context(), release, arch, logger)
			if err != nil {
				return fmt.Errorf("build node image: %w", err)
			}

			logger.V(0).Infof("Built %s", image)

			return nil
		},
	}

	cmd.Flags().StringVar(&eksVersion, "eks-version", "", "EKS version to build (default: latest supported)")
	cmd.Flags().StringVar(&arch, "arch", buildArch(), "target architecture (amd64 or arm64)")
	cmd.Flags().BoolVar(&listVersions, "list-versions", false, "list supported EKS versions and exit")

	return cmd
}

// buildArch returns the default node image architecture, matching the host.
func buildArch() string {
	return runtime.GOARCH
}
