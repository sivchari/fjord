package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/kind/pkg/log"

	"github.com/sivchari/fjord/internal/cluster"
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

// createClusterOptions carries the create cluster flag values.
type createClusterOptions struct {
	eksVersion string
	name       string
	buildLocal bool
	wait       time.Duration
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

	image := nodeimage.FloatingTag(release)

	if opts.buildLocal {
		logger.V(0).Infof("Building node image for EKS %s (%s) ...", release.EKSVersion, release.KubeVersion)

		image, err = nodeimage.Build(ctx, release, buildArch(), logger)
		if err != nil {
			return fmt.Errorf("build node image: %w", err)
		}
	}

	coreDNSRepo, coreDNSTag := coreDNSKubeadmRepository(release.CoreDNSImage)

	logger.V(0).Infof("Creating cluster %q (EKS %s / %s) ...", opts.name, release.EKSVersion, release.KubeVersion)

	provider := kind.NewProvider(logger)

	err = provider.CreateCluster(opts.name, kind.CreateOptions{
		NodeImage: image,
		Config: &kind.Config{
			Name:                   opts.name,
			KubeVersion:            release.KubeVersion,
			CoreDNSImageRepository: coreDNSRepo,
			CoreDNSImageTag:        coreDNSTag,
		},
		WaitForReady: opts.wait,
	})
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}

	if err := applyEKSDefaults(ctx, logger, provider, opts.name); err != nil {
		return err
	}

	logger.V(0).Infof("Cluster %q is ready. Set kubectl context to \"kind-%s\".", opts.name, opts.name)

	return nil
}

// applyEKSDefaults adjusts a freshly created cluster to the default state of
// a new EKS cluster (currently the gp2 default StorageClass).
func applyEKSDefaults(ctx context.Context, logger log.Logger, provider kind.Provider, name string) error {
	logger.V(0).Info("Applying EKS default state (gp2 StorageClass) ...")

	kubeconfig, err := provider.KubeConfig(name)
	if err != nil {
		return fmt.Errorf("get kubeconfig: %w", err)
	}

	client, err := cluster.NewClient(kubeconfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	if err := cluster.EnsureDefaultStorageClass(ctx, client); err != nil {
		return fmt.Errorf("ensure default storage class: %w", err)
	}

	return nil
}
