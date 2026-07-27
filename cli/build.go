package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"sigs.k8s.io/kind/pkg/log"

	fjord "github.com/sivchari/fjord"
	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/nodeimage"
)

// agentImageRepository is the GHCR repository fjord publishes the
// fjord-agent image under.
const agentImageRepository = "ghcr.io/sivchari/fjord/agent"

func newBuildCmd(logger log.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build an artifact",
	}
	cmd.AddCommand(newBuildNodeImageCmd(logger))
	cmd.AddCommand(newBuildAgentImageCmd(logger))

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

// defaultAgentImage returns the default fjord-agent image tag for the
// running fjord CLI version.
func defaultAgentImage() string {
	return fmt.Sprintf("%s:%s", agentImageRepository, fjord.Version)
}

func newBuildAgentImageCmd(logger log.Logger) *cobra.Command {
	var (
		arch  string
		local bool
	)

	cmd := &cobra.Command{
		Use:   "agent-image",
		Short: "Build the fjord-agent image (run from the repository root)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tag := defaultAgentImage()

			logger.V(0).Infof("Building fjord-agent image %s (%s) ...", tag, arch)

			if err := buildAgentImage(cmd.Context(), tag, arch, local, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}

			logger.V(0).Infof("Built %s", tag)

			return nil
		},
	}

	cmd.Flags().StringVar(&arch, "arch", buildArch(), "target architecture (amd64 or arm64)")
	cmd.Flags().BoolVar(&local, "local", false, "keep the built image in the local docker daemon instead of pushing it")

	return cmd
}

// buildAgentImage runs `docker build` to build the fjord-agent image tagged
// tag for arch, writing docker's output to stdout/stderr. local keeps the
// image only in the local docker daemon instead of pushing it to its
// registry.
func buildAgentImage(ctx context.Context, tag, arch string, local bool, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker")
	cmd.Args = append(cmd.Args, agentImageBuildArgs(tag, arch, local)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build agent image %q: %w", tag, err)
	}

	return nil
}

// agentImageBuildArgs returns the `docker build` arguments to build the
// fjord-agent image tagged tag for arch. local loads the built image into
// the local docker daemon; otherwise it is pushed to its registry.
func agentImageBuildArgs(tag, arch string, local bool) []string {
	args := []string{
		"build",
		"-f", "docker/Dockerfile",
		"--platform", "linux/" + arch,
		"-t", tag,
	}

	if local {
		args = append(args, "--load")
	} else {
		args = append(args, "--push")
	}

	return append(args, ".")
}
