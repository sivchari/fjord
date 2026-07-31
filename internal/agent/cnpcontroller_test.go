package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

// unstructuredHaroCNP builds the unstructured representation of haroCNP
// (the same fixture clusternetworkpolicy_test.go uses), as decoded from the
// wire by CNPController's dynamic informer.
func unstructuredHaroCNP() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": ClusterNetworkPolicyGroup + "/" + ClusterNetworkPolicyVersion,
		"kind":       ClusterNetworkPolicyKind,
		"metadata": map[string]any{
			"name": "haro-workspace-preview-port-envoy-gateway-only",
			"uid":  "cnp-uid-1",
		},
		"spec": map[string]any{
			"tier":     "Admin",
			"priority": int64(100),
			"subject": map[string]any{
				"namespaces": map[string]any{
					"matchExpressions": []any{
						map[string]any{"key": "haro.layerx.co.jp/user-id", "operator": "Exists"},
					},
				},
			},
			"ingress": []any{
				map[string]any{
					"name":   "deny-non-gateway-namespaces-to-preview-port",
					"action": "Deny",
					"from": []any{
						map[string]any{
							"namespaces": map[string]any{
								"matchExpressions": []any{
									map[string]any{
										"key":      "kubernetes.io/metadata.name",
										"operator": "NotIn",
										"values":   []any{"haro-system"},
									},
								},
							},
						},
					},
					"ports": []any{
						map[string]any{"portNumber": map[string]any{"protocol": "TCP", "port": int64(8080)}},
					},
				},
			},
		},
	}}
}

func TestDecodeClusterNetworkPolicy(t *testing.T) {
	t.Parallel()

	cnp, err := decodeClusterNetworkPolicy(unstructuredHaroCNP())
	if err != nil {
		t.Fatalf("decodeClusterNetworkPolicy: %v", err)
	}

	if cnp.Name != "haro-workspace-preview-port-envoy-gateway-only" {
		t.Errorf("Name = %q, want %q", cnp.Name, "haro-workspace-preview-port-envoy-gateway-only")
	}

	if cnp.Subject.Namespaces == nil || len(cnp.Subject.Namespaces.MatchExpressions) != 1 {
		t.Fatalf("Subject.Namespaces = %+v, want one match expression", cnp.Subject.Namespaces)
	}

	if got := cnp.Subject.Namespaces.MatchExpressions[0].Key; got != "haro.layerx.co.jp/user-id" {
		t.Errorf("Subject.Namespaces match expression key = %q, want %q", got, "haro.layerx.co.jp/user-id")
	}

	if len(cnp.Ingress) != 1 {
		t.Fatalf("len(Ingress) = %d, want 1", len(cnp.Ingress))
	}

	rule := cnp.Ingress[0]
	if rule.Action != CNPActionDeny {
		t.Errorf("Ingress[0].Action = %q, want %q", rule.Action, CNPActionDeny)
	}

	if len(rule.From) != 1 || rule.From[0].Namespaces == nil {
		t.Fatalf("Ingress[0].From = %+v, want one namespace peer", rule.From)
	}

	if len(rule.Ports) != 1 || rule.Ports[0].Protocol != corev1.ProtocolTCP || rule.Ports[0].Port != 8080 {
		t.Errorf("Ingress[0].Ports = %+v, want [{TCP 8080}]", rule.Ports)
	}
}

func TestNamespaceMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		selector *metav1.LabelSelector
		labels   map[string]string
		want     bool
	}{
		{
			name:     "nil selector matches nothing",
			selector: nil,
			labels:   map[string]string{"team": "a"},
			want:     false,
		},
		{
			name:     "empty selector matches everything",
			selector: &metav1.LabelSelector{},
			labels:   map[string]string{"team": "a"},
			want:     true,
		},
		{
			name:     "matchLabels hit",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
			labels:   map[string]string{"team": "a"},
			want:     true,
		},
		{
			name:     "matchLabels miss",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
			labels:   map[string]string{"team": "b"},
			want:     false,
		},
		{
			name: "matchExpressions Exists hit",
			selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "haro.layerx.co.jp/user-id", Operator: metav1.LabelSelectorOpExists},
			}},
			labels: map[string]string{"haro.layerx.co.jp/user-id": "abc"},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns", Labels: tt.labels}}

			got, err := namespaceMatches(tt.selector, ns)
			if err != nil {
				t.Fatalf("namespaceMatches: %v", err)
			}

			if got != tt.want {
				t.Errorf("namespaceMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDesiredNetworkPolicies(t *testing.T) {
	t.Parallel()

	cnp := decodedCNP{cnp: haroCNP(), uid: types.UID("cnp-uid-1")}

	matching := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "haro-workspace-preview-abc",
		Labels: map[string]string{"haro.layerx.co.jp/user-id": "abc"},
	}}
	nonMatching := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}

	var warnings []string

	logf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	desired, err := desiredNetworkPolicies([]decodedCNP{cnp}, []*corev1.Namespace{matching, nonMatching}, logf)
	if err != nil {
		t.Fatalf("desiredNetworkPolicies: %v", err)
	}

	if len(desired) != 1 {
		t.Fatalf("len(desired) = %d, want 1", len(desired))
	}

	np, ok := desired[desiredKey{namespace: matching.Name, name: NetworkPolicyName(cnp.cnp.Name)}]
	if !ok {
		t.Fatalf("desired does not contain a NetworkPolicy for namespace %q", matching.Name)
	}

	if len(np.OwnerReferences) != 1 {
		t.Fatalf("len(OwnerReferences) = %d, want 1", len(np.OwnerReferences))
	}

	owner := np.OwnerReferences[0]
	if owner.Kind != ClusterNetworkPolicyKind || owner.Name != cnp.cnp.Name || owner.UID != cnp.uid {
		t.Errorf("owner reference = %+v, want kind %q name %q uid %q", owner, ClusterNetworkPolicyKind, cnp.cnp.Name, cnp.uid)
	}

	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// TestDesiredNetworkPolicies_LogsUnsupported verifies desiredNetworkPolicies
// surfaces TranslateClusterNetworkPolicy's Unsupported entries via logf,
// rather than silently dropping them.
func TestDesiredNetworkPolicies_LogsUnsupported(t *testing.T) {
	t.Parallel()

	cnp := decodedCNP{cnp: ClusterNetworkPolicy{
		Name:    "pod-subject",
		Subject: CNPSubject{Namespaces: &metav1.LabelSelector{}, Pods: &metav1.LabelSelector{}},
		Ingress: []CNPRule{{Name: "allow-all", Action: CNPActionAllow}},
	}}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-ns"}}

	var warnings []string

	logf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	desired, err := desiredNetworkPolicies([]decodedCNP{cnp}, []*corev1.Namespace{ns}, logf)
	if err != nil {
		t.Fatalf("desiredNetworkPolicies: %v", err)
	}

	if len(desired) != 1 {
		t.Fatalf("len(desired) = %d, want 1", len(desired))
	}

	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
}

// TestCNPController_Reconcile verifies reconcile creates the NetworkPolicy a
// matching CNP+Namespace pair desires, then removes it once the CNP is
// deleted (an orphan with no corresponding desired entry).
func TestCNPController_Reconcile(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "haro-workspace-preview-abc",
		Labels: map[string]string{"haro.layerx.co.jp/user-id": "abc"},
	}}

	client := fake.NewClientset(ns)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)

	controller := NewCNPController(client, dynamicClient, t.Logf)

	if err := controller.nsInformer.GetStore().Add(ns); err != nil {
		t.Fatalf("add namespace to store: %v", err)
	}

	if err := controller.cnpInformer.GetStore().Add(unstructuredHaroCNP()); err != nil {
		t.Fatalf("add cluster network policy to store: %v", err)
	}

	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	npName := NetworkPolicyName("haro-workspace-preview-port-envoy-gateway-only")

	np, err := client.NetworkingV1().NetworkPolicies(ns.Name).Get(t.Context(), npName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get network policy %s/%s: %v", ns.Name, npName, err)
	}

	if np.Labels[ManagedByLabelKey] != ManagedByLabelValue {
		t.Errorf("managed-by label = %q, want %q", np.Labels[ManagedByLabelKey], ManagedByLabelValue)
	}

	// Deleting the CNP (removing it from the informer store, as the real
	// informer would once the API server reports its deletion) should make
	// the next reconcile pass delete the orphaned NetworkPolicy.
	if err := controller.cnpInformer.GetStore().Delete(unstructuredHaroCNP()); err != nil {
		t.Fatalf("remove cluster network policy from store: %v", err)
	}

	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile (after cnp deletion): %v", err)
	}

	_, err = client.NetworkingV1().NetworkPolicies(ns.Name).Get(t.Context(), npName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("get network policy after cnp deletion: err = %v, want NotFound", err)
	}
}

// TestCNPController_ReconcileIgnoresUnmanagedNetworkPolicies verifies
// reconcile never touches a NetworkPolicy it did not create itself (no
// ManagedByLabelKey label), even though it is not in the desired set.
func TestCNPController_ReconcileIgnoresUnmanagedNetworkPolicies(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app-ns"}}
	handWritten := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-written", Namespace: ns.Name},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}},
	}

	client := fake.NewClientset(ns, handWritten)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)

	controller := NewCNPController(client, dynamicClient, t.Logf)

	if err := controller.nsInformer.GetStore().Add(ns); err != nil {
		t.Fatalf("add namespace to store: %v", err)
	}

	if err := controller.reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, err := client.NetworkingV1().NetworkPolicies(ns.Name).Get(t.Context(), handWritten.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("hand-written network policy was removed: %v", err)
	}
}

// TestCNPController_Run verifies Run reconciles once its informers'
// initial caches sync, and returns promptly once its context is canceled
// -- i.e. it does not leak a goroutine.
func TestCNPController_Run(t *testing.T) {
	t.Parallel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "haro-workspace-preview-abc",
		Labels: map[string]string{"haro.layerx.co.jp/user-id": "abc"},
	}}

	client := fake.NewClientset(ns)

	gvr := schema.GroupVersionResource{Group: ClusterNetworkPolicyGroup, Version: ClusterNetworkPolicyVersion, Resource: ClusterNetworkPolicyPlural}
	listKinds := map[schema.GroupVersionResource]string{gvr: ClusterNetworkPolicyKind + "List"}
	// NewSimpleDynamicClientWithCustomListKinds registers listKinds' entries
	// into the scheme it is given, mutating it; passing the process-wide
	// scheme.Scheme here would race with every other parallel test reading
	// it (e.g. via NewSimpleDynamicClient), so this test uses its own.
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, unstructuredHaroCNP())

	controller := NewCNPController(client, dynamicClient, t.Logf)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() { done <- controller.Run(ctx) }()

	npName := NetworkPolicyName("haro-workspace-preview-port-envoy-gateway-only")

	err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := client.NetworkingV1().NetworkPolicies(ns.Name).Get(ctx, npName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("get network policy %s/%s: %w", ns.Name, npName, err)
		}

		return true, nil
	})
	if err != nil {
		t.Fatalf("wait for network policy: %v", err)
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
