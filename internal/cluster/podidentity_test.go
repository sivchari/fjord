package cluster

import (
	"context"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
)

func TestEnsurePodIdentity(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsurePodIdentity(context.Background(), client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:abc", "my-cluster"); err != nil {
		t.Fatalf("EnsurePodIdentity: %v", err)
	}

	assertPodIdentityDaemonSet(t, client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:abc", "my-cluster")
}

func TestEnsurePodIdentityIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	for range 2 {
		if err := EnsurePodIdentity(context.Background(), client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:abc", "my-cluster"); err != nil {
			t.Fatalf("EnsurePodIdentity: %v", err)
		}
	}

	assertPodIdentityDaemonSet(t, client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:abc", "my-cluster")
}

func TestEnsurePodIdentityUpdatesImageAndClusterName(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	if err := EnsurePodIdentity(context.Background(), client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:abc", "my-cluster"); err != nil {
		t.Fatalf("EnsurePodIdentity: %v", err)
	}

	if err := EnsurePodIdentity(context.Background(), client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:def", "other-cluster"); err != nil {
		t.Fatalf("EnsurePodIdentity: %v", err)
	}

	assertPodIdentityDaemonSet(t, client, "public.ecr.aws/eks/eks-pod-identity-agent@sha256:def", "other-cluster")
}

// assertPodIdentityDaemonSet verifies fjord's eks-pod-identity-agent
// DaemonSet exists in client with the shape EnsurePodIdentity must produce:
// hostNetwork, a privileged init container, a main container carrying only
// CAP_NET_BIND_SERVICE and bound to podIdentityAgentHostIP, image, and
// --cluster-name clusterName among its args.
func assertPodIdentityDaemonSet(t *testing.T, client *fake.Clientset, image, clusterName string) {
	t.Helper()

	ds, err := client.AppsV1().DaemonSets(agent.SystemNamespace).Get(context.Background(), podIdentityAgentName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}

	if !ds.Spec.Template.Spec.HostNetwork {
		t.Error("pod spec HostNetwork = false, want true")
	}

	if len(ds.Spec.Template.Spec.Tolerations) == 0 {
		t.Error("pod spec has no tolerations, want a toleration for every taint")
	}

	assertPodIdentityInitContainer(t, ds, image)
	assertPodIdentityMainContainer(t, ds, image, clusterName)
}

// assertPodIdentityInitContainer verifies the DaemonSet's single init
// container runs image with a privileged security context, matching the
// official chart's `initialize` initContainer.
func assertPodIdentityInitContainer(t *testing.T, ds *appsv1.DaemonSet, image string) {
	t.Helper()

	if len(ds.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("daemonset has %d init containers, want 1", len(ds.Spec.Template.Spec.InitContainers))
	}

	initContainer := ds.Spec.Template.Spec.InitContainers[0]
	if initContainer.Image != image {
		t.Errorf("init container image = %q, want %q", initContainer.Image, image)
	}

	if initContainer.SecurityContext == nil || initContainer.SecurityContext.Privileged == nil || !*initContainer.SecurityContext.Privileged {
		t.Errorf("init container security context = %+v, want privileged", initContainer.SecurityContext)
	}
}

// assertPodIdentityMainContainer verifies the DaemonSet's single main
// container runs image with the args, capabilities, and host port binding
// EnsurePodIdentity must produce.
func assertPodIdentityMainContainer(t *testing.T, ds *appsv1.DaemonSet, image, clusterName string) {
	t.Helper()

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("daemonset has %d containers, want 1", len(ds.Spec.Template.Spec.Containers))
	}

	container := ds.Spec.Template.Spec.Containers[0]

	if container.Image != image {
		t.Errorf("container image = %q, want %q", container.Image, image)
	}

	wantArgs := []string{
		"--port", "80",
		"--cluster-name", clusterName,
		"--probe-port", "2703",
		"--endpoint", "http://fjord-agent.kube-system.svc:8080",
		"--bind-hosts", podIdentityAgentHostIP,
	}
	if got := container.Args; !slices.Equal(got, wantArgs) {
		t.Errorf("container args = %v, want %v", got, wantArgs)
	}

	if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil ||
		!slices.Contains(container.SecurityContext.Capabilities.Add, "CAP_NET_BIND_SERVICE") {
		t.Errorf("container security context = %+v, want CAP_NET_BIND_SERVICE capability added", container.SecurityContext)
	}

	if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		t.Error("main container is privileged, want only the init container to be")
	}

	if len(container.Ports) != 1 || container.Ports[0].HostIP != podIdentityAgentHostIP || container.Ports[0].HostPort != podIdentityAgentPort {
		t.Errorf("container ports = %+v, want a single port bound to %s:%d", container.Ports, podIdentityAgentHostIP, podIdentityAgentPort)
	}
}
