//go:build integration

package integration

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// irsaNamespace, irsaSAName, and irsaRoleARN are kept distinct from
	// every other subtest's namespace/names so none of them collide.
	irsaNamespace = "irsa-e2e"
	irsaSAName    = "s3-reader"
	irsaRoleARN   = "arn:aws:iam::000000000000:role/s3-read-role"
	irsaPodName   = "irsa-e2e-pod"
	irsaContainer = "aws-cli"
)

// verifyIRSA is e2e② from plan-v1.md Phase 4: it creates a ServiceAccount
// carrying the eks.amazonaws.com/role-arn annotation, and verifies that `aws
// sts get-caller-identity` run from inside a pod using that ServiceAccount
// resolves to the assumed-role ARN. A successful run proves the full IRSA
// chain from plan-v1.md's Phase 4 design:
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
func verifyIRSA(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	ctx := t.Context()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: irsaNamespace}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	t.Cleanup(func() { deleteNamespace(t, client, irsaNamespace) })

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

	// Both webhooks must be serving before the pod is created: pod-identity-
	// webhook injects AWS_ROLE_ARN and the projected token, and fjord-agent's
	// injector adds AWS_ENDPOINT_URL_STS. The injector fails open, so if it
	// is not ready the pod is admitted without that override and the SDK
	// would reach real AWS instead of the fake STS.
	waitForDeploymentAvailable(t, client, "pod-identity-webhook")
	waitForDeploymentAvailable(t, client, "fjord-agent")

	pod := createSleepPod(t, client, irsaNamespace, irsaPodName, irsaSAName, awsCLIImage, irsaContainer)
	waitForPodRunning(t, client, pod.Namespace, pod.Name)

	out := runAWSGetCallerIdentity(t, pod.Namespace, pod.Name, irsaContainer)

	wantARNContains := "assumed-role/s3-read-role/"
	if !strings.Contains(out, wantARNContains) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantARNContains)
	}
}
