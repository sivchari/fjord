package cluster

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
)

func TestEnsureAuthenticatorRBAC(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureAuthenticatorRBAC(context.Background(), client); err != nil {
		t.Fatalf("EnsureAuthenticatorRBAC: %v", err)
	}

	assertAuthenticatorRBAC(t, client)
}

func TestEnsureAuthenticatorRBACIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	for range 2 {
		if err := EnsureAuthenticatorRBAC(context.Background(), client); err != nil {
			t.Fatalf("EnsureAuthenticatorRBAC: %v", err)
		}
	}

	assertAuthenticatorRBAC(t, client)
}

// assertAuthenticatorRBAC verifies fjord-authenticator's ServiceAccount and
// ClusterRoleBinding exist in client, and that the binding grants the same
// ClusterRole fjord-agent's own ServiceAccount uses.
func assertAuthenticatorRBAC(t *testing.T, client *fake.Clientset) {
	t.Helper()

	ctx := context.Background()

	if _, err := client.CoreV1().ServiceAccounts(agent.SystemNamespace).Get(ctx, AuthenticatorServiceAccountName, metav1.GetOptions{}); err != nil {
		t.Errorf("get service account: %v", err)
	}

	binding, err := client.RbacV1().ClusterRoleBindings().Get(ctx, authenticatorClusterRoleBindingName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cluster role binding: %v", err)
	}

	if binding.RoleRef.Name != agentName {
		t.Errorf("RoleRef.Name = %q, want %q", binding.RoleRef.Name, agentName)
	}

	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != AuthenticatorServiceAccountName || binding.Subjects[0].Namespace != agent.SystemNamespace {
		t.Errorf("Subjects = %+v, want a single ServiceAccount subject %q in %q", binding.Subjects, AuthenticatorServiceAccountName, agent.SystemNamespace)
	}
}

// TestAuthenticatorDaemonSetSchedulesOnAnyNode is a regression test for the
// DaemonSet carrying a node-role.kubernetes.io/control-plane nodeSelector,
// inherited from the kind backend. rask's single node does not carry that
// label, so the DaemonSet had no eligible node, nothing served the TokenReview
// webhook, and every access-entry login failed with "Unauthorized" (the API
// server logging "connection refused" dialling the webhook).
func TestAuthenticatorDaemonSetSchedulesOnAnyNode(t *testing.T) {
	t.Parallel()

	ds := authenticatorDaemonSet("example.com/agent:test")

	if got := ds.Spec.Template.Spec.NodeSelector; len(got) != 0 {
		t.Errorf("nodeSelector = %v, want none so the single rask node is eligible", got)
	}

	if !ds.Spec.Template.Spec.HostNetwork {
		t.Error("hostNetwork = false, want true: the webhook's server URL is localhost")
	}

	if len(ds.Spec.Template.Spec.Tolerations) == 0 {
		t.Error("tolerations = none, want at least one so a tainted node still runs it")
	}
}

// TestAuthenticatorDaemonSetMountsTLSSecret is a regression test for the
// DaemonSet hostPath-mounting /etc/fjord/authn for its TLS material, a kind
// concept: kind mounted a host directory into the node container, but rask
// delivers no files into its node this way, so the mount was always empty
// and fjord-authenticator exited immediately with "no such file or
// directory". It must instead mount AuthenticatorTLSCertName as a Secret.
func TestAuthenticatorDaemonSetMountsTLSSecret(t *testing.T) {
	t.Parallel()

	ds := authenticatorDaemonSet("example.com/agent:test")

	spec := ds.Spec.Template.Spec

	if len(spec.Volumes) != 1 {
		t.Fatalf("len(Volumes) = %d, want 1", len(spec.Volumes))
	}

	volume := spec.Volumes[0]
	if volume.HostPath != nil {
		t.Errorf("Volumes[0].HostPath = %+v, want nil: the TLS material must come from a Secret, not a hostPath", volume.HostPath)
	}

	if volume.Secret == nil || volume.Secret.SecretName != AuthenticatorTLSCertName {
		t.Errorf("Volumes[0].Secret = %+v, want SecretName %q", volume.Secret, AuthenticatorTLSCertName)
	}

	if len(spec.Containers) != 1 {
		t.Fatalf("len(Containers) = %d, want 1", len(spec.Containers))
	}

	mounts := spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].Name != volume.Name || mounts[0].MountPath != authenticatorTLSMountPath {
		t.Errorf("VolumeMounts = %+v, want a single mount of %q at %q", mounts, volume.Name, authenticatorTLSMountPath)
	}
}

// TestEnsureAuthenticatorCreatesTLSSecret verifies EnsureAuthenticator
// stores cert in AuthenticatorTLSCertName, so the DaemonSet's Secret volume
// (see TestAuthenticatorDaemonSetMountsTLSSecret) resolves to real TLS
// material.
func TestEnsureAuthenticatorCreatesTLSSecret(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	ca := testCA(t)

	cert, err := ca.IssueServerCert("fjord-authenticator", []string{"localhost"})
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}

	if err := EnsureAuthenticator(context.Background(), client, "example.com/agent:test", cert); err != nil {
		t.Fatalf("EnsureAuthenticator: %v", err)
	}

	assertTLSSecret(t, client, AuthenticatorTLSCertName, ca)
}
