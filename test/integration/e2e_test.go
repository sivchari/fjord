//go:build integration

// Package integration contains end-to-end tests that build the fjord CLI,
// create a real cluster with docker, and verify EKS parity from the inside.
//
// TestCreateCluster creates a single shared cluster (~2 minutes) and runs
// every verification -- v0's EKS parity checks plus v1's AWS auth emulation
// chains (IRSA, IMDS, EKS Pod Identity, access-entry authentication) -- as
// its subtests, so the cluster is paid for once instead of once per feature.
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	clusterName   = "fjord-e2e"
	eksVersion    = "1.33"
	createTimeout = 25 * time.Minute

	// kubeContext is the kubeconfig context rask wrote for clusterName: rask
	// names a cluster's context exactly its cluster name.
	kubeContext = clusterName

	// agentHostPort is the host port fjord-agent's fake STS API and EKS API
	// facade are published on, matching cluster.AgentNodePort (rask's
	// hostproc runtime shares the host network namespace, so this NodePort
	// is reachable directly on the host).
	agentHostPort = "30080"

	// awsSTSEndpoint is the in-cluster DNS name and port pods reach
	// fjord-agent's fake STS API through, matching
	// internal/cluster/agent.go's Service in the kube-system namespace.
	awsSTSEndpoint = "http://fjord-agent.kube-system.svc:8080"

	// awsCLIImage bundles the aws CLI, used to exercise the AWS credential
	// chain (IRSA, IMDS, EKS Pod Identity) from inside a pod.
	awsCLIImage = "amazon/aws-cli:latest"

	// podWaitTimeout bounds how long subtests wait for a pod to reach
	// Running.
	podWaitTimeout = 3 * time.Minute

	// execRetryTimeout bounds how long subtests retry a kubectl exec while
	// the target container is still starting up.
	execRetryTimeout = 2 * time.Minute
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
	// --build-local builds the agent image with `docker build -f
	// docker/Dockerfile .`, so run from the repository root where that
	// Dockerfile resolves.
	create.Dir = "../.."
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

	t.Run("V0", func(t *testing.T) {
		gitVersion := serverGitVersion(t, client)
		if !strings.Contains(gitVersion, "-eks-") {
			t.Errorf("server gitVersion = %q, want it to contain %q", gitVersion, "-eks-")
		}

		assertDefaultStorageClass(t, client)
		assertKubeProxyImage(t, client)
		assertCoreDNSImage(t, client)
	})

	t.Run("Agent", func(t *testing.T) {
		verifyAgentSTSGetCallerIdentity(t, bin)
	})

	t.Run("IRSA", func(t *testing.T) {
		verifyIRSA(t, client)
	})

	t.Run("IMDS", func(t *testing.T) {
		verifyIMDS(t, client)
	})

	t.Run("PodIdentity", func(t *testing.T) {
		verifyPodIdentity(t, client)
	})

	t.Run("AccessEntries", func(t *testing.T) {
		verifyAccessEntries(t, bin)
	})
}

// assertCoreDNSImage verifies CoreDNS runs the EKS-D image and becomes ready,
// proving the kubeadm dns.imageRepository override took effect and the image
// is actually pullable. Node readiness alone does not guarantee this.
func assertCoreDNSImage(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Minute)

	for {
		deploy, err := client.AppsV1().Deployments("kube-system").Get(t.Context(), "coredns", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get coredns deployment: %v", err)
		}

		image := deploy.Spec.Template.Spec.Containers[0].Image
		if !strings.Contains(image, "eks-distro/coredns") {
			t.Fatalf("coredns image = %q, want it to contain %q", image, "eks-distro/coredns")
		}

		if deploy.Status.ReadyReplicas >= 1 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("coredns not ready: %d/%d replicas (image %q)", deploy.Status.ReadyReplicas, deploy.Status.Replicas, image)
		}

		time.Sleep(5 * time.Second)
	}
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

// newClient builds a Kubernetes client from the kubeconfig context rask
// wrote for the e2e cluster.
func newClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}

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

// createSleepPod creates a pod named name in namespace, running
// serviceAccount (or the namespace's default ServiceAccount, if empty) and
// image, sleeping so kubectlExec/runAWSGetCallerIdentity can run commands
// inside it.
func createSleepPod(t *testing.T, client kubernetes.Interface, namespace, name, serviceAccount, image, container string) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{
				{Name: container, Image: image, Command: []string{"sleep", "3600"}},
			},
		},
	}

	created, err := client.CoreV1().Pods(namespace).Create(t.Context(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	return created
}

// waitForPodRunning blocks until namespace/name reaches PodRunning, or fails
// the test after podWaitTimeout.
func waitForPodRunning(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, podWaitTimeout, true, func(ctx context.Context) (bool, error) {
		pod, getErr := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, fmt.Errorf("get pod %s/%s: %w", namespace, name, getErr)
		}

		return pod.Status.Phase == corev1.PodRunning, nil
	})
	if err != nil {
		t.Fatalf("wait for pod %s/%s running: %v", namespace, name, err)
	}
}

// waitForDeploymentAvailable blocks until the named kube-system Deployment
// reports at least one available replica, so callers whose flow depends on a
// mutating webhook (e.g. IRSA needs pod-identity-webhook) do not race its
// rollout.
func waitForDeploymentAvailable(t *testing.T, client kubernetes.Interface, name string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, podWaitTimeout, true, func(ctx context.Context) (bool, error) {
		deploy, getErr := client.AppsV1().Deployments("kube-system").Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return false, fmt.Errorf("get deployment kube-system/%s: %w", name, getErr)
		}

		return deploy.Status.AvailableReplicas >= 1, nil
	})
	if err != nil {
		t.Fatalf("wait for deployment kube-system/%s available: %v", name, err)
	}
}

// deleteNamespace deletes namespace (best-effort; failures are logged, not
// fatal, since this only runs during test cleanup).
func deleteNamespace(t *testing.T, client kubernetes.Interface, namespace string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}); err != nil {
		t.Logf("delete namespace %q: %v", namespace, err)
	}
}

// deletePod deletes namespace/name (best-effort; see deleteNamespace).
func deletePod(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Logf("delete pod %s/%s: %v", namespace, name, err)
	}
}

// deleteServiceAccount deletes namespace/name (best-effort; see
// deleteNamespace).
func deleteServiceAccount(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Logf("delete service account %s/%s: %v", namespace, name, err)
	}
}

// kubectlExec runs `kubectl exec` for kubeContext against namespace/pod's
// container, running args as the in-container command, and returns its
// combined output.
func kubectlExec(namespace, pod, container string, args ...string) (string, error) {
	cmdArgs := []string{"--context", kubeContext, "exec", "-n", namespace, pod, "-c", container, "--"}
	cmdArgs = append(cmdArgs, args...)

	out, err := exec.Command("kubectl", cmdArgs...).CombinedOutput()

	return string(out), err
}

// retryKubectlExec retries kubectlExec until it succeeds or timeout elapses,
// covering the short window after a pod reaches Running where its container
// runtime may not yet be ready to serve `kubectl exec`.
func retryKubectlExec(t *testing.T, timeout time.Duration, namespace, pod, container string, args ...string) string {
	t.Helper()

	deadline := time.Now().Add(timeout)

	var out string

	var err error

	for {
		out, err = kubectlExec(namespace, pod, container, args...)
		if err == nil {
			return out
		}

		if time.Now().After(deadline) {
			t.Fatalf("kubectl exec %v: %v\n%s", args, err, out)
		}

		time.Sleep(3 * time.Second)
	}
}

// runAWSGetCallerIdentity runs `aws sts get-caller-identity` inside
// namespace/pod's container via kubectl exec, overriding AWS_REGION and
// AWS_ENDPOINT_URL_STS (plus any extraEnv) for that single invocation, and
// retrying while the aws-cli container is still starting up. It returns
// stdout+stderr.
func runAWSGetCallerIdentity(t *testing.T, namespace, pod, container string, extraEnv ...string) string {
	t.Helper()

	env := append([]string{"AWS_REGION=us-east-1", "AWS_ENDPOINT_URL_STS=" + awsSTSEndpoint}, extraEnv...)
	args := append([]string{"env"}, env...)
	args = append(args, "aws", "sts", "get-caller-identity")

	return retryKubectlExec(t, execRetryTimeout, namespace, pod, container, args...)
}
