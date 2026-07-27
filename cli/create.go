package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/kind"
	"github.com/sivchari/fjord/internal/nodeimage"
)

const defaultClusterName = "fjord"

func newCreateCmd(logger log.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a resource",
	}
	cmd.AddCommand(newCreateClusterCmd(logger))

	return cmd
}

func newCreateClusterCmd(logger log.Logger) *cobra.Command {
	var (
		eksVersion string
		name       string
		buildLocal bool
		wait       time.Duration
	)

	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Create an EKS-compatible local cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if eksVersion == "" {
				eksVersion = latestVersion(eksd.SupportedVersions())
			}

			release, err := eksd.Resolve(eksVersion)
			if err != nil {
				return fmt.Errorf("resolve eks version: %w", err)
			}

			image := nodeimage.FloatingTag(release)
			if buildLocal {
				logger.V(0).Infof("Building node image for EKS %s (%s) ...", release.EKSVersion, release.KubeVersion)

				image, err = nodeimage.Build(cmd.Context(), release, buildArch(), logger)
				if err != nil {
					return fmt.Errorf("build node image: %w", err)
				}
			}

			coreDNSRepo, coreDNSTag := splitImageRef(release.CoreDNSImage)

			logger.V(0).Infof("Creating cluster %q (EKS %s / %s) ...", name, release.EKSVersion, release.KubeVersion)

			provider := kind.NewProvider(logger)
			err = provider.CreateCluster(name, kind.CreateOptions{
				NodeImage: image,
				Config: &kind.Config{
					Name:                   name,
					CoreDNSImageRepository: coreDNSRepo,
					CoreDNSImageTag:        coreDNSTag,
				},
				WaitForReady: wait,
			})
			if err != nil {
				return fmt.Errorf("create cluster: %w", err)
			}

			logger.V(0).Infof("Cluster %q is ready. Set kubectl context to \"kind-%s\".", name, name)

			return nil
		},
	}

	cmd.Flags().StringVar(&eksVersion, "eks-version", "", "EKS version to emulate (default: latest supported)")
	cmd.Flags().StringVar(&name, "name", defaultClusterName, "cluster name")
	cmd.Flags().BoolVar(&buildLocal, "build-local", false, "build the node image locally instead of pulling the prebuilt image")
	cmd.Flags().DurationVar(&wait, "wait", 2*time.Minute, "wait for the control plane to be ready (0 to disable)")

	return cmd
}
