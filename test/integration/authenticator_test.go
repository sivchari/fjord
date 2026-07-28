//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	// viewerPrincipalName is granted the View access policy and is
	// expected to authenticate and read successfully but not write.
	viewerPrincipalName = "viewer"
	// nobodyPrincipalName is registered as a principal but never granted
	// an access entry, and is expected to fail authentication entirely.
	nobodyPrincipalName = "nobody"
)

// verifyAccessEntries is e2e⑤ from plan-v1.md Phase 7: it registers two
// principals via `fjord create principal`, grants one of them the View
// access policy via `fjord grant access-entry`, and points kubectl at both
// via `fjord update-kubeconfig`. A successful run proves the full
// authentication chain from plan-v1.md's Phase 7 design:
//
//  1. kube-apiserver's authentication-token-webhook-config-file points at
//     fjord-authenticator's static pod (internal/authn.Stage), reachable at
//     127.0.0.1 because both run hostNetwork in the control-plane node's
//     network namespace.
//  2. fjord-authenticator (internal/agent.Authenticator) resolves the
//     token's access key id against the principal registry and its access
//     entry, returning the principal's Kubernetes identity. A principal
//     with no access entry does not authenticate at all.
//  3. AssociateAccessPolicy (internal/agent/facade.go) materialized a
//     ClusterRoleBinding binding the principal's synthetic access-entry
//     group to the built-in "view" ClusterRole.
//
// It verifies: a read (list pods) succeeds for the viewer principal, a
// write (create a namespace) is forbidden for it, and the principal with no
// access entry is rejected as unauthenticated.
func verifyAccessEntries(t *testing.T, bin string) {
	t.Helper()

	createPrincipal(t, bin, viewerPrincipalName)
	grantViewAccessEntry(t, bin, viewerPrincipalName)
	viewerContext := updateKubeconfig(t, bin, viewerPrincipalName)

	createPrincipal(t, bin, nobodyPrincipalName)
	nobodyContext := updateKubeconfig(t, bin, nobodyPrincipalName)

	assertKubectlSucceeds(t, viewerContext, "get", "pods")
	assertKubectlForbidden(t, viewerContext, "create", "namespace", "test-x")
	assertKubectlUnauthorized(t, nobodyContext)
}

// createPrincipal registers name via `fjord create principal`, cleaning it
// up from the fjord-principals registry afterward.
func createPrincipal(t *testing.T, bin, name string) {
	t.Helper()

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "principal", name, "--name", clusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete principal %q: %v\n%s", name, err, out)
		}
	})

	cmd := exec.Command(bin, "create", "principal", name, "--name", clusterName)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create principal %q: %v\n%s", name, err, out)
	}
}

// grantViewAccessEntry grants principal the View access policy via `fjord
// grant access-entry`.
func grantViewAccessEntry(t *testing.T, bin, principal string) {
	t.Helper()

	cmd := exec.Command(bin, "grant", "access-entry",
		"--name", clusterName,
		"--principal", principal,
		"--policy", "View",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grant access-entry for %q: %v\n%s", principal, err, out)
	}
}

// updateKubeconfig runs `fjord update-kubeconfig` for principal and returns
// the kubeconfig context name it added ("fjord-<principal>@<cluster>"),
// registering a cleanup that removes the context (and its Cluster/AuthInfo
// entries) from the default kubeconfig, since update-kubeconfig writes to
// the user's real ~/.kube/config.
func updateKubeconfig(t *testing.T, bin, principal string) string {
	t.Helper()

	contextName := "fjord-" + principal + "@" + clusterName

	t.Cleanup(func() { removeKubeconfigContext(t, contextName) })

	cmd := exec.Command(bin, "update-kubeconfig", "--name", clusterName, "--principal", principal)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-kubeconfig for %q: %v\n%s", principal, err, out)
	}

	return contextName
}

// removeKubeconfigContext removes contextName's Context, Cluster, and
// AuthInfo entries from the default kubeconfig (best-effort; failures are
// logged, not fatal, since this only runs during test cleanup).
func removeKubeconfigContext(t *testing.T, contextName string) {
	t.Helper()

	if out, err := exec.Command("kubectl", "config", "delete-context", contextName).CombinedOutput(); err != nil {
		t.Logf("kubectl config delete-context %q: %v\n%s", contextName, err, out)
	}

	if out, err := exec.Command("kubectl", "config", "unset", "clusters."+contextName).CombinedOutput(); err != nil {
		t.Logf("kubectl config unset clusters.%s: %v\n%s", contextName, err, out)
	}

	if out, err := exec.Command("kubectl", "config", "unset", "users."+contextName).CombinedOutput(); err != nil {
		t.Logf("kubectl config unset users.%s: %v\n%s", contextName, err, out)
	}
}

// assertKubectlSucceeds runs `kubectl --context kubeContext args...` and
// fails the test if it exits non-zero.
func assertKubectlSucceeds(t *testing.T, kubeContext string, args ...string) {
	t.Helper()

	cmdArgs := append([]string{"--context", kubeContext}, args...)
	if out, err := exec.Command("kubectl", cmdArgs...).CombinedOutput(); err != nil {
		t.Errorf("kubectl %v = %v, want success\n%s", args, err, out)
	}
}

// assertKubectlForbidden runs `kubectl --context kubeContext args...` and
// fails the test unless it fails with a Forbidden error.
func assertKubectlForbidden(t *testing.T, kubeContext string, args ...string) {
	t.Helper()

	cmdArgs := append([]string{"--context", kubeContext}, args...)

	out, err := exec.Command("kubectl", cmdArgs...).CombinedOutput()
	if err == nil {
		t.Errorf("kubectl %v succeeded, want a Forbidden error\n%s", args, out)

		return
	}

	if !strings.Contains(string(out), "Forbidden") {
		t.Errorf("kubectl %v output = %s, want it to contain %q", args, out, "Forbidden")
	}
}

// assertKubectlUnauthorized runs `kubectl --context kubeContext get pods`
// and fails the test unless it fails with an Unauthorized error.
func assertKubectlUnauthorized(t *testing.T, kubeContext string) {
	t.Helper()

	out, err := exec.Command("kubectl", "--context", kubeContext, "get", "pods").CombinedOutput()
	if err == nil {
		t.Errorf("kubectl get pods with no access entry succeeded, want an Unauthorized error\n%s", out)

		return
	}

	if !strings.Contains(string(out), "Unauthorized") {
		t.Errorf("kubectl get pods output = %s, want it to contain %q", out, "Unauthorized")
	}
}
