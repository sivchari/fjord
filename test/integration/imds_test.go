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
	// imdsNamespace and imdsPodName are kept distinct from every other
	// subtest's namespace/names so none of them collide.
	imdsNamespace = "imds-e2e"
	imdsPodName   = "imds-e2e-pod"
	imdsContainer = "aws-cli"

	// imdsNodeRole is the node role name fjord-imds advertises, matching
	// cli's default (`create cluster` below does not override
	// --node-role-name).
	imdsNodeRole = "fjord-node-role"

	// imdsMetadataEndpoint is the link-local address fjord-imds serves
	// IMDSv2 on inside the node's network namespace (see
	// internal/cluster/imds.go's DaemonSet).
	imdsMetadataEndpoint = "http://169.254.169.254"
)

// verifyIMDS is e2e③ from plan-v1.md Phase 5: it verifies that a plain pod
// carrying no eks.amazonaws.com/role-arn annotation can still resolve AWS
// credentials from the default credential chain's IMDSv2 provider, and that
// those credentials resolve to the node role's assumed-role ARN via `aws sts
// get-caller-identity`. A successful run proves the full IMDS chain from
// plan-v1.md's Phase 5 design:
//
//  1. fjord-imds (internal/cluster.EnsureIMDS's DaemonSet) added
//     169.254.169.254 to the node's loopback interface and served IMDSv2
//     there.
//  2. internal/agent.IMDS issued node-role credentials and registered
//     their access key in the fjord-principals Secret under the node
//     role's assumed-role ARN.
//  3. fjord-agent's fake STS (internal/agent/sts.go) resolved
//     GetCallerIdentity for that access key to the same assumed-role ARN,
//     proving IMDS and the fake STS share one principal registry across
//     their separate processes.
func verifyIMDS(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: imdsNamespace}}
	if _, err := client.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	t.Cleanup(func() { deleteNamespace(t, client, imdsNamespace) })

	// No ServiceAccount is set, so the pod is never mutated by
	// pod-identity-webhook or fjord's own injector, and only IMDS can
	// supply it credentials.
	pod := createSleepPod(t, client, imdsNamespace, imdsPodName, "", awsCLIImage, imdsContainer)
	waitForPodRunning(t, client, pod.Namespace, pod.Name)

	out := runAWSGetCallerIdentity(t, pod.Namespace, pod.Name, imdsContainer,
		"AWS_EC2_METADATA_SERVICE_ENDPOINT="+imdsMetadataEndpoint)

	wantARNContains := "assumed-role/" + imdsNodeRole + "/"
	if !strings.Contains(out, wantARNContains) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantARNContains)
	}
}
