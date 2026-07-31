package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// lbNode builds a Node with a single address of typ (corev1.NodeInternalIP
// or corev1.NodeExternalIP) set to ip.
func lbNode(name string, typ corev1.NodeAddressType, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: typ, Address: ip}}},
	}
}

// lbService builds a "web"-named type: LoadBalancer Service, optionally
// with a loadBalancerClass and/or a pre-populated ingress.
func lbService(class *string, ingress []corev1.LoadBalancerIngress) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: class,
			Ports:             []corev1.ServicePort{{Port: 80}},
		},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: ingress}},
	}
}

// loadBalancerReconcileTestCase is a single
// TestLoadBalancerController_Reconcile case.
type loadBalancerReconcileTestCase struct {
	name        string
	svc         *corev1.Service
	wantIngress []corev1.LoadBalancerIngress
}

// loadBalancerReconcileTestCases holds
// TestLoadBalancerController_Reconcile's cases, factored out as a
// package-level value so the test function itself stays short.
var loadBalancerReconcileTestCases = []loadBalancerReconcileTestCase{
	{
		name:        "claims a class-less load balancer service",
		svc:         lbService(nil, nil),
		wantIngress: []corev1.LoadBalancerIngress{{IP: "10.0.0.5"}},
	},
	{
		name: "skips a non load balancer service",
		svc: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
		},
		wantIngress: nil,
	},
	{
		name:        "skips a service with a loadBalancerClass",
		svc:         lbService(ptr("eks.amazonaws.com/nlb"), nil),
		wantIngress: nil,
	},
	{
		name:        "does not touch a service with a foreign ingress already set",
		svc:         lbService(nil, []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}}),
		wantIngress: []corev1.LoadBalancerIngress{{IP: "192.168.1.100"}},
	},
}

// ptr returns a pointer to v, for building loadBalancerClass values inline.
func ptr(v string) *string { return &v }

func TestDesiredIngress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []*corev1.Node
		want  []corev1.LoadBalancerIngress
	}{
		{
			name:  "single node internal ip",
			nodes: []*corev1.Node{lbNode("node-1", corev1.NodeInternalIP, "10.0.0.5")},
			want:  []corev1.LoadBalancerIngress{{IP: "10.0.0.5"}},
		},
		{
			name:  "external ip fallback when no internal ip",
			nodes: []*corev1.Node{lbNode("node-1", corev1.NodeExternalIP, "203.0.113.5")},
			want:  []corev1.LoadBalancerIngress{{IP: "203.0.113.5"}},
		},
		{
			name: "multi node dedup and sort",
			nodes: []*corev1.Node{
				lbNode("node-2", corev1.NodeInternalIP, "10.0.0.9"),
				lbNode("node-1", corev1.NodeInternalIP, "10.0.0.5"),
				lbNode("node-1-dup", corev1.NodeInternalIP, "10.0.0.5"),
			},
			want: []corev1.LoadBalancerIngress{{IP: "10.0.0.5"}, {IP: "10.0.0.9"}},
		},
		{
			name:  "node with no address is skipped",
			nodes: []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := desiredIngress(tt.nodes)
			if len(got) != len(tt.want) {
				t.Fatalf("desiredIngress() = %v, want %v", got, tt.want)
			}

			for i := range got {
				if got[i].IP != tt.want[i].IP {
					t.Errorf("desiredIngress()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestLoadBalancerController_Reconcile covers reconcile's per-Service
// eligibility rules against a fixed single-node cluster.
func TestLoadBalancerController_Reconcile(t *testing.T) {
	t.Parallel()

	for _, tt := range loadBalancerReconcileTestCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			node := lbNode("node-1", corev1.NodeInternalIP, "10.0.0.5")
			client := fake.NewClientset(tt.svc, node)
			c := NewLoadBalancerController(client, t.Logf)

			if err := c.svcInformer.GetStore().Add(tt.svc); err != nil {
				t.Fatalf("add service to store: %v", err)
			}

			if err := c.nodeInformer.GetStore().Add(node); err != nil {
				t.Fatalf("add node to store: %v", err)
			}

			if err := c.reconcile(t.Context()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			got, err := client.CoreV1().Services("default").Get(t.Context(), tt.svc.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get service: %v", err)
			}

			assertIngressEqual(t, got.Status.LoadBalancer.Ingress, tt.wantIngress)
		})
	}
}

// TestLoadBalancerController_ReconcileIdempotent verifies reconcile does not
// issue an API write when a Service's ingress already equals what
// reconcile would set.
func TestLoadBalancerController_ReconcileIdempotent(t *testing.T) {
	t.Parallel()

	node := lbNode("node-1", corev1.NodeInternalIP, "10.0.0.5")
	svc := lbService(nil, []corev1.LoadBalancerIngress{{IP: "10.0.0.5"}})

	client := fake.NewClientset(svc, node)

	var wrote bool

	client.PrependReactor("update", "services", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		wrote = true

		return false, nil, nil
	})

	c := NewLoadBalancerController(client, t.Logf)

	if err := c.svcInformer.GetStore().Add(svc); err != nil {
		t.Fatalf("add service to store: %v", err)
	}

	if err := c.nodeInformer.GetStore().Add(node); err != nil {
		t.Fatalf("add node to store: %v", err)
	}

	if err := c.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if wrote {
		t.Error("reconcile issued an API write for a Service whose ingress already matched")
	}
}

// TestLoadBalancerController_Run verifies Run claims a class-less
// LoadBalancer Service once its informers' initial caches sync, and returns
// promptly once its context is canceled -- i.e. it does not leak a
// goroutine.
func TestLoadBalancerController_Run(t *testing.T) {
	t.Parallel()

	node := lbNode("node-1", corev1.NodeInternalIP, "10.0.0.5")
	svc := lbService(nil, nil)

	client := fake.NewClientset(node, svc)
	c := NewLoadBalancerController(client, t.Logf)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- c.Run(ctx) }()

	err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		got, err := client.CoreV1().Services("default").Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get service: %w", err)
		}

		return len(got.Status.LoadBalancer.Ingress) != 0, nil
	})
	if err != nil {
		t.Fatalf("wait for service to be claimed: %v", err)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was canceled (goroutine leak)")
	}
}

// assertIngressEqual fails t if got and want do not hold the same
// LoadBalancerIngress entries, in order.
func assertIngressEqual(t *testing.T, got, want []corev1.LoadBalancerIngress) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("ingress = %v, want %v", got, want)
	}

	for i := range got {
		if got[i].IP != want[i].IP {
			t.Errorf("ingress[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
