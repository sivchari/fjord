package cluster

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/sivchari/fjord/internal/cluster/manifests"
)

// enforcerRESTMapper returns a RESTMapper resolving every kind
// manifests.KubeNetworkPolicies applies (all built-in kinds present on every
// cluster), standing in for a real cluster's discovery-derived RESTMapper.
func enforcerRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ServiceAccount"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}, meta.RESTScopeNamespace)

	return mapper
}

// TestApplyManifestKubeNetworkPolicies is a regression test verifying the
// embedded kube-network-policies manifest decodes and applies cleanly,
// creates every object the enforcer needs, and keeps its image pinned by
// digest (so an upstream re-vendor cannot silently drift to a mutable tag).
func TestApplyManifestKubeNetworkPolicies(t *testing.T) {
	t.Parallel()

	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := enforcerRESTMapper()

	if err := applyManifest(t.Context(), dynamicClient, mapper, manifests.KubeNetworkPolicies); err != nil {
		t.Fatalf("applyManifest(kube-network-policies.yaml): %v", err)
	}

	const name = "kube-network-policies"

	assertAppliedGVR(t, dynamicClient, schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, "", name)
	assertAppliedGVR(t, dynamicClient, schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, "", name)
	assertAppliedGVR(t, dynamicClient, schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}, "kube-system", name)

	daemonSetGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}

	assertAppliedGVR(t, dynamicClient, daemonSetGVR, "kube-system", name)

	obj, err := dynamicClient.Resource(daemonSetGVR).Namespace("kube-system").Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonsets %q: %v", name, err)
	}

	containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		t.Fatalf("NestedSlice(spec.template.spec.containers) = %v, %v, %v", containers, found, err)
	}

	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("containers[0] is %T, want map[string]any", containers[0])
	}

	image, ok := container["image"].(string)
	if !ok {
		t.Fatalf("containers[0].image is %T, want string", container["image"])
	}

	if !strings.HasPrefix(image, "registry.k8s.io/networking/kube-network-policies@sha256:") {
		t.Errorf("image = %q, want a digest-pinned registry.k8s.io/networking/kube-network-policies reference", image)
	}
}
