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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

	client := newClient(t)

	gitVersion := serverGitVersion(t, client)
	if !strings.Contains(gitVersion, "-eks-") {
		t.Errorf("server gitVersion = %q, want it to contain %q", gitVersion, "-eks-")
	}

	assertDefaultStorageClass(t, client)
	assertKubeProxyImage(t, client)
}

// assertDefaultStorageClass verifies exactly one default StorageClass exists
// and that it is gp2, matching a new EKS cluster.
func assertDefaultStorageClass(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	scs, err := client.StorageV1().StorageClasses().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list storage classes: %v", err)
	}

	var defaults []string

	for _, sc := range scs.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			defaults = append(defaults, sc.Name)
		}
	}

	if len(defaults) != 1 || defaults[0] != "gp2" {
		t.Errorf("default storage classes = %v, want exactly [gp2]", defaults)
	}
}

// assertKubeProxyImage verifies the kube-proxy DaemonSet runs the EKS-D build.
func assertKubeProxyImage(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	ds, err := client.AppsV1().DaemonSets("kube-system").Get(t.Context(), "kube-proxy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get kube-proxy daemonset: %v", err)
	}

	image := ds.Spec.Template.Spec.Containers[0].Image
	if !strings.Contains(image, "-eks-") {
		t.Errorf("kube-proxy image = %q, want it to contain %q", image, "-eks-")
	}
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

// newClient builds a Kubernetes client from the kubeconfig context kind
// wrote for the e2e cluster.
func newClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + clusterName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create kubernetes client: %v", err)
	}

	return client
}

// serverGitVersion returns the API server's reported gitVersion.
func serverGitVersion(t *testing.T, client kubernetes.Interface) string {
	t.Helper()

	version, err := client.Discovery().ServerVersion()
	if err != nil {
		t.Fatalf("get server version: %v", err)
	}

	return version.GitVersion
}
