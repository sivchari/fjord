package rask

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	raskcluster "github.com/sivchari/rask/pkg/cluster"

	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// authWebhookConfigFileArg is the kube-apiserver flag name (kubeadm-style,
// no leading "--") that authWebhookArg builds an ExtraAPIServerArgs entry
// for.
const authWebhookConfigFileArg = "authentication-token-webhook-config-file"

// waitOption maps CreateOptions.WaitForReady onto rask's Options.Wait.
// rask's Wait is a coarse node/coredns barrier, not a duration: zero (the
// default, waiting disabled) maps to raskcluster.WaitNode, the cheapest
// condition rask offers; any positive duration maps to
// raskcluster.WaitCoreDNS, since fjord always waits for the cluster before
// applying its own manifests and CoreDNS readiness is the closer signal.
// The duration value itself is not honored — rask has no caller-supplied
// timeout for either condition — so callers relying on WaitForReady to
// bound how long Create can block should not rely on this provider for
// that guarantee.
func waitOption(waitForReady time.Duration) string {
	if waitForReady > 0 {
		return raskcluster.WaitCoreDNS
	}

	return raskcluster.WaitNode
}

// coreDNSImage joins c's CoreDNSImageRepository and CoreDNSImageTag into
// rask's single Options.CoreDNSImage value ("repository:tag"). Both empty
// leaves rask's own default in place ("" is returned). Exactly one set is
// rejected: rask has no way to override only the repository or only the
// tag, and guessing the other half would silently pick an image the caller
// never asked for.
func coreDNSImage(c *clusterprovider.Config) (string, error) {
	switch {
	case c.CoreDNSImageRepository == "" && c.CoreDNSImageTag == "":
		return "", nil
	case c.CoreDNSImageRepository == "" || c.CoreDNSImageTag == "":
		return "", fmt.Errorf("coredns image repository and tag must both be set or both be empty (got repository=%q tag=%q)",
			c.CoreDNSImageRepository, c.CoreDNSImageTag)
	default:
		return c.CoreDNSImageRepository + ":" + c.CoreDNSImageTag, nil
	}
}

// mountDestPrefix disambiguates the rask PrebootFile.Dest paths staged from
// different Config.ExtraMounts entries, so files with the same relative
// path under two different mounts' HostPath don't collide once flattened
// into rask's single preboot directory.
func mountDestPrefix(i int) string {
	return fmt.Sprintf("mount%d", i)
}

// prebootFiles walks every host directory in mounts and returns one rask
// PrebootFile per regular file found, so the whole tree Config.ExtraMounts
// describes is delivered into rask's cluster data directory before any
// cluster process starts.
func prebootFiles(mounts []clusterprovider.Mount) ([]raskcluster.PrebootFile, error) {
	var files []raskcluster.PrebootFile

	for i, m := range mounts {
		walkErr := filepath.WalkDir(m.HostPath, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				return nil
			}

			rel, err := filepath.Rel(m.HostPath, p)
			if err != nil {
				return fmt.Errorf("relative path for %q under %q: %w", p, m.HostPath, err)
			}

			files = append(files, raskcluster.PrebootFile{
				Src:  p,
				Dest: path.Join(mountDestPrefix(i), filepath.ToSlash(rel)),
			})

			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("stage extra mount %q: %w", m.HostPath, walkErr)
		}
	}

	return files, nil
}

// authWebhookDest locates the preboot destination (a PrebootFile.Dest, i.e.
// relative to the cluster's preboot directory) of the webhook kubeconfig
// kube-apiserver must read.
//
// w.ConfigFilePath is a path inside the API server container's mount
// vocabulary; it locates the owning mount by matching w.VolumeMountPath
// (the directory that path is documented to live under) against each
// mount's ContainerPath, then rebuilds the equivalent dest using the same
// mountDestPrefix scheme prebootFiles used for that mount. An unmatched
// VolumeMountPath is reported as an error rather than guessed at, since it
// means Config.AuthWebhook and Config.ExtraMounts disagree about where the
// config file lives.
//
// Turning that dest into a path the API server can actually open is rask's
// job (Provider.PrebootPath): it is a host path under hostproc, where the
// host is the node, and an in-guest path under vz, where it is not.
// Computing it here would bake in the hostproc answer and hand the vz guest
// a macOS path it cannot open.
func authWebhookDest(w *clusterprovider.AuthWebhook, mounts []clusterprovider.Mount) (string, error) {
	for i, m := range mounts {
		if m.ContainerPath != w.VolumeMountPath {
			continue
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(w.ConfigFilePath, w.VolumeMountPath), "/")

		return path.Join(mountDestPrefix(i), filepath.ToSlash(rel)), nil
	}

	return "", fmt.Errorf("auth webhook config file %q: no extra mount with container path %q", w.ConfigFilePath, w.VolumeMountPath)
}
