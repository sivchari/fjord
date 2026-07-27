//go:build integration

// Package integration contains end-to-end tests that build the fjord CLI,
// create a real cluster with docker, and verify EKS parity from the inside.
package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	clusterName   = "fjord-e2e"
	eksVersion    = "1.33"
	createTimeout = 25 * time.Minute
)

func TestCreateCluster(t *testing.T) {
	bin := buildCLI(t)

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "cluster", "--name", clusterName)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})

	create := exec.Command(bin, "create", "cluster",
		"--eks-version", eksVersion,
		"--name", clusterName,
		"--build-local",
	)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr

	done := make(chan error, 1)
	if err := create.Start(); err != nil {
		t.Fatalf("start create cluster: %v", err)
	}

	go func() { done <- create.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("create cluster: %v", err)
		}
	case <-time.After(createTimeout):
		_ = create.Process.Kill()
		t.Fatalf("create cluster timed out after %s", createTimeout)
	}

	gitVersion := serverGitVersion(t)
	if !strings.Contains(gitVersion, "-eks-") {
		t.Errorf("server gitVersion = %q, want it to contain %q", gitVersion, "-eks-")
	}

	// TODO(phase 6): assert exactly one default StorageClass named gp2.
	// TODO(phase 6): assert the kube-proxy DaemonSet image tag contains -eks-.
}

// buildCLI compiles the fjord binary into a temp dir and returns its path.
func buildCLI(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "fjord")

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/fjord")
	cmd.Dir = "../.."

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fjord: %v\n%s", err, out)
	}

	return bin
}

// serverGitVersion returns the API server's reported gitVersion using the
// kubeconfig context kind wrote for the e2e cluster.
func serverGitVersion(t *testing.T) string {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + clusterName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}

	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		t.Fatalf("create discovery client: %v", err)
	}

	version, err := client.ServerVersion()
	if err != nil {
		t.Fatalf("get server version: %v", err)
	}

	return version.GitVersion
}
