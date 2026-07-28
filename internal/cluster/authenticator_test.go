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
