//go:build integration

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// podIdentityNamespace is "default": EKS Pod Identity associations are
	// looked up by (namespace, serviceAccount), and the aws CLI's
	// create-pod-identity-association call below is bound to this
	// namespace.
	podIdentityNamespace = "default"
	podIdentitySAName    = "pod-id-sa"
	podIdentityRoleARN   = "arn:aws:iam::000000000000:role/pod-id-role"
	podIdentityPodName   = "pod-identity-e2e-pod"
	podIdentityContainer = "aws-cli"
)

// verifyPodIdentity is e2e④ from plan-v1.md Phase 6: it registers an EKS
// Pod Identity association for a ServiceAccount via `aws eks
// create-pod-identity-association` against fjord-agent's EKS API facade, and
// verifies that `aws sts get-caller-identity` run from inside a pod using
// that ServiceAccount resolves to the assumed-role ARN. A successful run
// proves the full Pod Identity chain from plan-v1.md's Phase 6 design:
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
//  3. fjord-agent's EKS Auth API emulation (internal/agent/eksauth.go)
//     authenticated the token via TokenReview, resolved it to the
//     registered PodIdentityAssociation, and issued credentials for its
//     role.
func verifyPodIdentity(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: podIdentitySAName, Namespace: podIdentityNamespace}}
	if _, err := client.CoreV1().ServiceAccounts(podIdentityNamespace).Create(t.Context(), sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service account: %v", err)
	}

	t.Cleanup(func() { deleteServiceAccount(t, client, podIdentityNamespace, podIdentitySAName) })

	createPodIdentityAssociation(t)

	pod := createSleepPod(t, client, podIdentityNamespace, podIdentityPodName, podIdentitySAName, awsCLIImage, podIdentityContainer)

	t.Cleanup(func() { deletePod(t, client, podIdentityNamespace, podIdentityPodName) })

	waitForPodRunning(t, client, pod.Namespace, pod.Name)

	out := runAWSGetCallerIdentity(t, pod.Namespace, pod.Name, podIdentityContainer)

	wantARNContains := "assumed-role/pod-id-role/"
	if !strings.Contains(out, wantARNContains) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantARNContains)
	}
}

// createPodIdentityAssociation runs `aws eks create-pod-identity-association`
// from the host against fjord-agent's EKS API facade (published on
// agentHostPort by its NodePort Service), the same command real tooling
// would run to register a ServiceAccount's IAM role.
func createPodIdentityAssociation(t *testing.T) {
	t.Helper()

	cmd := exec.Command("aws", "eks", "create-pod-identity-association",
		"--endpoint-url", "http://localhost:"+agentHostPort,
		"--cluster-name", clusterName,
		"--namespace", podIdentityNamespace,
		"--service-account", podIdentitySAName,
		"--role-arn", podIdentityRoleARN,
	)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID=fjord-e2e",
		"AWS_SECRET_ACCESS_KEY=fake",
		"AWS_REGION=us-east-1",
	)

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("aws eks create-pod-identity-association: %v\n%s", err, out)
	}
}
