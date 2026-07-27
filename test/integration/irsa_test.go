//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// irsaClusterName is the cluster name used by the IRSA e2e test, kept
	// distinct from TestCreateCluster's and TestAgentSTSGetCallerIdentity's
	// cluster names so none of them collide if run concurrently.
	irsaClusterName = clusterName + "-irsa"

	irsaNamespace   = "irsa-e2e"
	irsaSAName      = "irsa-e2e-sa"
	irsaRoleARN     = "arn:aws:iam::000000000000:role/irsa-e2e-role"
	irsaPodName     = "irsa-e2e-pod"
	irsaContainer   = "aws-cli"
	irsaWaitTimeout = 3 * time.Minute

	// awsCLIImage bundles the aws CLI, used to exercise
	// AssumeRoleWithWebIdentity from inside a pod.
	awsCLIImage = "amazon/aws-cli:2.17.62"
)

// TestIRSAGetCallerIdentity is e2e② from plan-v1.md Phase 4: it creates a
// cluster with fjord-agent auth (and therefore IRSA) enabled, creates a
// ServiceAccount carrying the eks.amazonaws.com/role-arn annotation, and
// verifies that `aws sts get-caller-identity` run from inside a pod using
// that ServiceAccount resolves to the assumed-role ARN. A successful run
// proves the full IRSA chain from plan-v1.md's Phase 4 design:
//
//  1. amazon-eks-pod-identity-webhook injected AWS_ROLE_ARN,
//     AWS_WEB_IDENTITY_TOKEN_FILE, and the projected ServiceAccount token
//     volume into the pod, because its ServiceAccount carries the
//     eks.amazonaws.com/role-arn annotation.
//  2. fjord's own injector webhook (internal/agent.Injector) additionally
//     injected AWS_ENDPOINT_URL_STS, pointing the AWS SDK's
//     AssumeRoleWithWebIdentity call at fjord-agent's fake STS instead of
//     the real one.
//  3. fjord-agent's fake STS (internal/agent/sts.go) accepted the call
//     without verifying the web identity token and returned credentials
//     for the assumed role.
//
// This builds a full cluster (~tens of minutes) and requires pulling the aws
// CLI image, so it is not run as part of the default suite. Run it
// explicitly:
//
//	go test -C test -tags=integration -run TestIRSAGetCallerIdentity -v ./integration/...
func TestIRSAGetCallerIdentity(t *testing.T) {
	t.Skip("run explicitly: builds a full cluster with fjord-agent auth (IRSA) enabled")

	bin := buildCLI(t)

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "cluster", "--name", irsaClusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})

	create := exec.Command(bin, "create", "cluster",
		"--eks-version", eksVersion,
		"--name", irsaClusterName,
		"--build-local",
	)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr

	if err := create.Run(); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	config, client := irsaClientConfig(t)

	createIRSANamespaceAndServiceAccount(t, client)

	pod := createIRSAPod(t, client)
	waitForPodRunning(t, client, pod.Namespace, pod.Name)

	out := execInPod(t, config, client, pod.Namespace, pod.Name,
		[]string{"aws", "sts", "get-caller-identity", "--region", "us-east-1"})

	wantARNContains := "assumed-role/irsa-e2e-role/"
	if !strings.Contains(out, wantARNContains) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantARNContains)
	}
}

// irsaClientConfig builds a *rest.Config and Kubernetes client from the
// kubeconfig context kind wrote for the IRSA e2e cluster.
func irsaClientConfig(t *testing.T) (*rest.Config, kubernetes.Interface) {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + irsaClusterName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("create kubernetes client: %v", err)
	}

	return config, client
}

// createIRSANamespaceAndServiceAccount creates irsaNamespace and, inside it,
// a ServiceAccount carrying the eks.amazonaws.com/role-arn annotation that
// triggers both amazon-eks-pod-identity-webhook and fjord's injector.
func createIRSANamespaceAndServiceAccount(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	ctx := t.Context()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: irsaNamespace}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        irsaSAName,
			Namespace:   irsaNamespace,
			Annotations: map[string]string{"eks.amazonaws.com/role-arn": irsaRoleARN},
		},
	}
	if _, err := client.CoreV1().ServiceAccounts(irsaNamespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service account: %v", err)
	}
}

// createIRSAPod creates a pod running irsaSAName's ServiceAccount, sleeping
// so execInPod can run `aws sts get-caller-identity` inside it.
func createIRSAPod(t *testing.T, client kubernetes.Interface) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: irsaPodName, Namespace: irsaNamespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: irsaSAName,
			Containers: []corev1.Container{
				{Name: irsaContainer, Image: awsCLIImage, Command: []string{"sleep", "3600"}},
			},
		},
	}

	created, err := client.CoreV1().Pods(irsaNamespace).Create(t.Context(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	return created
}

// waitForPodRunning blocks until namespace/name reaches PodRunning, or fails
// the test after irsaWaitTimeout.
func waitForPodRunning(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, irsaWaitTimeout, true, func(ctx context.Context) (bool, error) {
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

// execInPod runs command inside namespace/name's irsaContainer and returns
// its stdout.
func execInPod(t *testing.T, config *rest.Config, client kubernetes.Interface, namespace, name string, command []string) string {
	t.Helper()

	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: irsaContainer,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}

	var stdout, stderr bytes.Buffer

	err = executor.StreamWithContext(t.Context(), remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("exec %v: %v\nstderr: %s", command, err, stderr.String())
	}

	return stdout.String()
}
