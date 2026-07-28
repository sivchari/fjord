package cluster

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
)

func TestEnsureKumo(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureKumo(context.Background(), client, "ghcr.io/sivchari/kumo:latest"); err != nil {
		t.Fatalf("EnsureKumo: %v", err)
	}

	assertKumoResources(t, client, "ghcr.io/sivchari/kumo:latest")
}

func TestEnsureKumoIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	for range 2 {
		if err := EnsureKumo(context.Background(), client, "ghcr.io/sivchari/kumo:latest"); err != nil {
			t.Fatalf("EnsureKumo: %v", err)
		}
	}

	assertKumoResources(t, client, "ghcr.io/sivchari/kumo:latest")
}

func TestEnsureKumoUpdatesImage(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureKumo(context.Background(), client, "ghcr.io/sivchari/kumo:v1"); err != nil {
		t.Fatalf("EnsureKumo: %v", err)
	}

	if err := EnsureKumo(context.Background(), client, "ghcr.io/sivchari/kumo:v2"); err != nil {
		t.Fatalf("EnsureKumo: %v", err)
	}

	assertKumoResources(t, client, "ghcr.io/sivchari/kumo:v2")
}

// assertKumoResources verifies kumo's Deployment and Service exist in
// client with the shape EnsureKumo must produce: a single container running
// image, listening on kumoPort, and a ClusterIP Service exposing that same
// port.
func assertKumoResources(t *testing.T, client *fake.Clientset, image string) {
	t.Helper()

	ctx := context.Background()

	deployment, err := client.AppsV1().Deployments(agent.SystemNamespace).Get(ctx, kumoName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}

	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("deployment has %d containers, want 1", len(deployment.Spec.Template.Spec.Containers))
	}

	container := deployment.Spec.Template.Spec.Containers[0]

	if container.Image != image {
		t.Errorf("deployment image = %q, want %q", container.Image, image)
	}

	wantArgs := []string{"--host", "0.0.0.0", "--port", "4566"}
	if got := container.Args; !slices.Equal(got, wantArgs) {
		t.Errorf("container args = %v, want %v", got, wantArgs)
	}

	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != kumoPort {
		t.Errorf("container ports = %+v, want a single port %d", container.Ports, kumoPort)
	}

	svc, err := client.CoreV1().Services(agent.SystemNamespace).Get(ctx, kumoName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}

	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != kumoPort {
		t.Errorf("service ports = %+v, want single port %d", svc.Spec.Ports, kumoPort)
	}
}
