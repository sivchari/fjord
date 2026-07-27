package cluster

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
)

func TestEnsureAgent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureAgent(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v1"); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	assertAgentResources(t, client, "ghcr.io/sivchari/fjord/agent:v1")
}

func TestEnsureAgentIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	for range 2 {
		if err := EnsureAgent(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v1"); err != nil {
			t.Fatalf("EnsureAgent: %v", err)
		}
	}

	assertAgentResources(t, client, "ghcr.io/sivchari/fjord/agent:v1")
}

func TestEnsureAgentUpdatesImage(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureAgent(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v1"); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	if err := EnsureAgent(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v2"); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}

	assertAgentResources(t, client, "ghcr.io/sivchari/fjord/agent:v2")
}

// assertAgentResources verifies every resource EnsureAgent creates exists
// in client with the expected shape, and that the Deployment runs image.
func assertAgentResources(t *testing.T, client *fake.Clientset, image string) {
	t.Helper()

	ctx := context.Background()

	if _, err := client.CoreV1().ServiceAccounts(agent.SystemNamespace).Get(ctx, agentName, metav1.GetOptions{}); err != nil {
		t.Errorf("get service account: %v", err)
	}

	if _, err := client.RbacV1().ClusterRoles().Get(ctx, agentName, metav1.GetOptions{}); err != nil {
		t.Errorf("get cluster role: %v", err)
	}

	if _, err := client.RbacV1().ClusterRoleBindings().Get(ctx, agentName, metav1.GetOptions{}); err != nil {
		t.Errorf("get cluster role binding: %v", err)
	}

	deployment, err := client.AppsV1().Deployments(agent.SystemNamespace).Get(ctx, agentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("deployment has %d containers, want 1", len(deployment.Spec.Template.Spec.Containers))
	}

	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != image {
		t.Errorf("deployment image = %q, want %q", got, image)
	}

	if got := deployment.Spec.Template.Spec.ServiceAccountName; got != agentName {
		t.Errorf("deployment service account = %q, want %q", got, agentName)
	}

	svc, err := client.CoreV1().Services(agent.SystemNamespace).Get(ctx, agentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}

	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != agentPort {
		t.Errorf("service ports = %+v, want single port %d", svc.Spec.Ports, agentPort)
	}

	nodePortSvc, err := client.CoreV1().Services(agent.SystemNamespace).Get(ctx, agentNodePortServiceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get nodeport service: %v", err)
	}

	if len(nodePortSvc.Spec.Ports) != 1 || nodePortSvc.Spec.Ports[0].NodePort != agentNodePort {
		t.Errorf("nodeport service ports = %+v, want NodePort %d", nodePortSvc.Spec.Ports, agentNodePort)
	}
}
