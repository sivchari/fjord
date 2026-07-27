//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// podIdentityClusterName is the cluster name used by the EKS Pod
	// Identity e2e test, kept distinct from every other e2e test's cluster
	// name so none of them collide if run concurrently.
	podIdentityClusterName = clusterName + "-podidentity"

	// podIdentityAgentHostPort is the host port fjord-agent's fake STS API
	// (and, on the same mux, its EKS Pod Identity facade endpoint) is
	// published on for this test.
	podIdentityAgentHostPort = "48082"

	podIdentityNamespace   = "pod-identity-e2e"
	podIdentitySAName      = "pod-identity-e2e-sa"
	podIdentityRoleARN     = "arn:aws:iam::000000000000:role/pod-identity-e2e-role"
	podIdentityPodName     = "pod-identity-e2e-pod"
	podIdentityContainer   = "aws-cli"
	podIdentityWaitTimeout = 3 * time.Minute
)

// createPodIdentityAssociationRequest mirrors
// internal/agent.createPodIdentityAssociationRequest; it is redeclared here
// (rather than importing the unexported internal type) because this test
// only needs to encode it as JSON.
type createPodIdentityAssociationRequest struct {
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount"`
	RoleARN        string `json:"roleArn"`
}

// TestPodIdentityGetCallerIdentity is e2e④ from plan-v1.md Phase 6: it
// creates a cluster with fjord-agent auth (and therefore EKS Pod Identity)
// enabled, registers a PodIdentityAssociation for a ServiceAccount via
// fjord-agent's facade endpoint, and verifies that `aws sts
// get-caller-identity` run from inside a pod using that ServiceAccount
// resolves to the assumed-role ARN. A successful run proves the full Pod
// Identity chain from plan-v1.md's Phase 6 design:
//
//  1. fjord's own Injector webhook (internal/agent.Injector) injected
//     AWS_CONTAINER_CREDENTIALS_FULL_URI and a projected ServiceAccount
//     token volume into the pod, because its ServiceAccount has a
//     PodIdentityAssociation registered.
//  2. The official eks-pod-identity-agent DaemonSet
//     (internal/cluster.EnsurePodIdentity), listening on
//     169.254.170.23, received the pod's credential request, exchanged the
//     projected token for credentials by calling its --endpoint override
//     (fjord-agent's EKS Auth API emulation) instead of the real EKS Auth
//     API, and returned them to the pod.
//  3. fjord-agent's EKS Auth API emulation
//     (internal/agent/eksauth.go) authenticated the token via TokenReview,
//     resolved it to the registered PodIdentityAssociation, and issued
//     credentials for its role.
//
// This builds a full cluster (~tens of minutes) and requires the `aws` CLI
// and pulling the aws CLI image, so it is not run as part of the default
// suite. Run it explicitly:
//
//	go test -C test -tags=integration -run TestPodIdentityGetCallerIdentity -v ./integration/...
func TestPodIdentityGetCallerIdentity(t *testing.T) {
	t.Skip("run explicitly: builds a full cluster with fjord-agent auth (EKS Pod Identity) enabled")

	bin := buildCLI(t)

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "cluster", "--name", podIdentityClusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})

	create := exec.Command(bin, "create", "cluster",
		"--eks-version", eksVersion,
		"--name", podIdentityClusterName,
		"--build-local",
		"--agent-host-port", podIdentityAgentHostPort,
	)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr

	if err := create.Run(); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	config, client := podIdentityClientConfig(t)

	createPodIdentityNamespaceAndServiceAccount(t, client)
	createPodIdentityAssociation(t)

	pod := createPodIdentityPod(t, client)
	waitForPodRunning(t, client, pod.Namespace, pod.Name)

	out := execInPod(t, config, client, pod.Namespace, pod.Name,
		[]string{"aws", "sts", "get-caller-identity", "--region", "us-east-1"})

	wantARNContains := "assumed-role/pod-identity-e2e-role/"
	if !strings.Contains(out, wantARNContains) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantARNContains)
	}
}

// podIdentityClientConfig builds a *rest.Config and Kubernetes client from
// the kubeconfig context kind wrote for the EKS Pod Identity e2e cluster.
func podIdentityClientConfig(t *testing.T) (*rest.Config, kubernetes.Interface) {
	t.Helper()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + podIdentityClusterName}

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

// createPodIdentityNamespaceAndServiceAccount creates podIdentityNamespace
// and, inside it, a plain ServiceAccount carrying no annotations: EKS Pod
// Identity association is looked up by (namespace, name), not by an
// annotation like IRSA's role-arn.
func createPodIdentityNamespaceAndServiceAccount(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	ctx := t.Context()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: podIdentityNamespace}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: podIdentitySAName, Namespace: podIdentityNamespace}}
	if _, err := client.CoreV1().ServiceAccounts(podIdentityNamespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service account: %v", err)
	}
}

// createPodIdentityAssociation registers podIdentitySAName's association via
// fjord-agent's facade endpoint (published on podIdentityAgentHostPort by
// the same NodePort Service the fake STS API uses), the equivalent of `aws
// eks create-pod-identity-association`.
func createPodIdentityAssociation(t *testing.T) {
	t.Helper()

	body, err := json.Marshal(createPodIdentityAssociationRequest{
		Namespace:      podIdentityNamespace,
		ServiceAccount: podIdentitySAName,
		RoleARN:        podIdentityRoleARN,
	})
	if err != nil {
		t.Fatalf("marshal create pod identity association request: %v", err)
	}

	url := fmt.Sprintf("http://localhost:%s/clusters/%s/pod-identity-associations", podIdentityAgentHostPort, podIdentityClusterName)

	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create pod identity association: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create pod identity association status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// createPodIdentityPod creates a pod running podIdentitySAName's
// ServiceAccount, sleeping so execInPod can run `aws sts
// get-caller-identity` inside it.
func createPodIdentityPod(t *testing.T, client kubernetes.Interface) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podIdentityPodName, Namespace: podIdentityNamespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: podIdentitySAName,
			Containers: []corev1.Container{
				{Name: podIdentityContainer, Image: awsCLIImage, Command: []string{"sleep", "3600"}},
			},
		},
	}

	created, err := client.CoreV1().Pods(podIdentityNamespace).Create(t.Context(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	return created
}
