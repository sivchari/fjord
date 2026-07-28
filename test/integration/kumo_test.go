//go:build integration

package integration

import (
	"testing"
)

// TestKumoSecretsManager is a skeleton for the kumo integration verification
// described in the fjord/kumo integration design: with --with-kumo, an
// IAM-identity pod's AWS SDK calls to a service kumo emulates (here,
// Secrets Manager) should actually reach kumo and succeed, not merely be
// admitted by the API server. It is skipped because it needs its own
// cluster built with --with-kumo (TestCreateCluster's shared cluster in
// e2e_test.go does not pass that flag), which the maintainer runs manually.
//
// Intended flow once unskipped:
//
//  1. Build the fjord CLI (see buildCLI) and run `fjord create cluster
//     --with-kumo --build-local --name <a distinct cluster name>`, which:
//     - deploys kumo to kube-system (internal/cluster.EnsureKumo)
//     - deploys fjord-agent with --aws-endpoint-url pointed at
//     cluster.KumoEndpoint, so its injector webhook
//     (internal/agent.Injector) injects AWS_ENDPOINT_URL into every pod
//     it also injects AWS_ENDPOINT_URL_STS or Pod Identity credentials
//     into.
//  2. Create a ServiceAccount carrying either the
//     eks.amazonaws.com/role-arn annotation (IRSA) or a registered
//     PodIdentityAssociation (`aws eks create-pod-identity-association`,
//     as verifyPodIdentity in podidentity_test.go does), so the pod using
//     it receives an IAM identity and therefore AWS_ENDPOINT_URL.
//  3. Create a pod (see createSleepPod) using that ServiceAccount, running
//     the aws CLI image, and wait for it to reach Running (see
//     waitForPodRunning).
//  4. From inside the pod (see kubectlExec/retryKubectlExec), run:
//     `aws secretsmanager create-secret --name <name> --secret-string
//     <value>` followed by `aws secretsmanager get-secret-value --secret-id
//     <name>`, with no --endpoint-url override, relying solely on the
//     injected AWS_ENDPOINT_URL to route the calls to kumo.
//  5. Assert get-secret-value's output contains the secret string that was
//     just written, proving the round trip actually reached kumo's
//     in-memory Secrets Manager storage rather than failing silently or
//     hitting real AWS.
//  6. Cleanup: delete the pod, ServiceAccount, and (via `fjord delete
//     cluster`) the cluster itself.
func TestKumoSecretsManager(t *testing.T) {
	t.Skip("skeleton: needs its own --with-kumo cluster; run manually")
}
