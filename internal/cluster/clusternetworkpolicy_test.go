package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/sivchari/fjord/internal/cluster/manifests"
)

// crdRESTMapper returns a RESTMapper resolving the
// apiextensions.k8s.io/v1 CustomResourceDefinition kind
// manifests.ClusterNetworkPolicyCRD applies as, standing in for a real
// cluster's discovery-derived RESTMapper (CustomResourceDefinition is a
// built-in kind present on every cluster, unlike the CRD it establishes).
func crdRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, meta.RESTScopeRoot)

	return mapper
}

// TestApplyManifestClusterNetworkPolicyCRD is a regression test verifying
// the embedded CRD manifest (manifests.ClusterNetworkPolicyCRD) decodes and
// applies cleanly, and declares the fields TranslateClusterNetworkPolicy
// reads (see internal/agent).
func TestApplyManifestClusterNetworkPolicyCRD(t *testing.T) {
	t.Parallel()

	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := crdRESTMapper()

	if err := applyManifest(t.Context(), dynamicClient, mapper, manifests.ClusterNetworkPolicyCRD); err != nil {
		t.Fatalf("applyManifest(clusternetworkpolicy-crd.yaml): %v", err)
	}

	const crdName = "clusternetworkpolicies.networking.k8s.aws"

	gvr := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	assertAppliedGVR(t, dynamicClient, gvr, "", crdName)

	obj, err := dynamicClient.Resource(gvr).Get(t.Context(), crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s %q: %v", gvr.Resource, crdName, err)
	}

	scope, found, err := unstructured.NestedString(obj.Object, "spec", "scope")
	if err != nil || !found {
		t.Fatalf("NestedString(spec.scope) = %v, %v, %v", scope, found, err)
	}

	if scope != "Cluster" {
		t.Errorf("spec.scope = %q, want %q", scope, "Cluster")
	}

	group, found, err := unstructured.NestedString(obj.Object, "spec", "group")
	if err != nil || !found {
		t.Fatalf("NestedString(spec.group) = %v, %v, %v", group, found, err)
	}

	if group != "networking.k8s.aws" {
		t.Errorf("spec.group = %q, want %q", group, "networking.k8s.aws")
	}
}
