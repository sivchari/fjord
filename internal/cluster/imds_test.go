package cluster

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
)

func TestEnsureIMDS(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureIMDS(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v1", "fjord-node-role"); err != nil {
		t.Fatalf("EnsureIMDS: %v", err)
	}

	assertIMDSDaemonSet(t, client, "ghcr.io/sivchari/fjord/agent:v1", "fjord-node-role")
}

func TestEnsureIMDSIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	for range 2 {
		if err := EnsureIMDS(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v1", "fjord-node-role"); err != nil {
			t.Fatalf("EnsureIMDS: %v", err)
		}
	}

	assertIMDSDaemonSet(t, client, "ghcr.io/sivchari/fjord/agent:v1", "fjord-node-role")
}

func TestEnsureIMDSUpdatesImageAndRoleName(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsureIMDS(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v1", "fjord-node-role"); err != nil {
		t.Fatalf("EnsureIMDS: %v", err)
	}

	if err := EnsureIMDS(context.Background(), client, "ghcr.io/sivchari/fjord/agent:v2", "custom-node-role"); err != nil {
		t.Fatalf("EnsureIMDS: %v", err)
	}

	assertIMDSDaemonSet(t, client, "ghcr.io/sivchari/fjord/agent:v2", "custom-node-role")
}

// assertIMDSDaemonSet verifies fjord-imds's DaemonSet exists in client with
// the shape EnsureIMDS must produce: hostNetwork, NET_ADMIN, image, and
// --node-role-name nodeRoleName among its args.
func assertIMDSDaemonSet(t *testing.T, client *fake.Clientset, image, nodeRoleName string) {
	t.Helper()

	ds, err := client.AppsV1().DaemonSets(agent.SystemNamespace).Get(context.Background(), imdsName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("daemonset has %d containers, want 1", len(ds.Spec.Template.Spec.Containers))
	}

	container := ds.Spec.Template.Spec.Containers[0]

	if container.Image != image {
		t.Errorf("container image = %q, want %q", container.Image, image)
	}

	if !ds.Spec.Template.Spec.HostNetwork {
		t.Error("pod spec HostNetwork = false, want true")
	}

	wantArgs := []string{"serve", "imds", "--port", "80", "--node-role-name", nodeRoleName}
	if got := container.Args; !slices.Equal(got, wantArgs) {
		t.Errorf("container args = %v, want %v", got, wantArgs)
	}

	if got := ds.Spec.Template.Spec.ServiceAccountName; got != agentName {
		t.Errorf("service account = %q, want %q", got, agentName)
	}

	if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil ||
		!slices.Contains(container.SecurityContext.Capabilities.Add, "NET_ADMIN") {
		t.Errorf("container security context = %+v, want NET_ADMIN capability added", container.SecurityContext)
	}

	if len(ds.Spec.Template.Spec.Tolerations) == 0 {
		t.Error("pod spec has no tolerations, want a toleration for every taint")
	}
}
