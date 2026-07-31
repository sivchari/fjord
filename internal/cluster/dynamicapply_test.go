package cluster

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

// testRESTMapper returns a RESTMapper resolving the core/v1 Namespace and
// ConfigMap kinds applyManifest's test fixtures use, mirroring what a real
// cluster's discovery-derived RESTMapper would resolve them to without
// requiring a live API server.
func testRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)

	return mapper
}

func newTestDynamicClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
}

func TestApplyManifest(t *testing.T) {
	t.Parallel()

	manifest := []byte(`
apiVersion: v1
kind: Namespace
metadata:
  name: fjord-test
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: fjord-config
  namespace: fjord-test
data:
  key: value
`)

	dynamicClient := newTestDynamicClient()
	mapper := testRESTMapper()

	if err := applyManifest(t.Context(), dynamicClient, mapper, manifest); err != nil {
		t.Fatalf("applyManifest: %v", err)
	}

	assertApplied(t, dynamicClient, "", "namespaces", "fjord-test")
	assertApplied(t, dynamicClient, "fjord-test", "configmaps", "fjord-config")
}

// TestApplyManifestSkipsEmptyDocuments verifies leading/trailing/duplicated
// "---" document separators (which decode to an empty Unstructured) do not
// error out, since real manifests (including metallb's) commonly have them.
func TestApplyManifestSkipsEmptyDocuments(t *testing.T) {
	t.Parallel()

	manifest := []byte(`
---
apiVersion: v1
kind: Namespace
metadata:
  name: fjord-test
---
---
`)

	dynamicClient := newTestDynamicClient()
	mapper := testRESTMapper()

	if err := applyManifest(t.Context(), dynamicClient, mapper, manifest); err != nil {
		t.Fatalf("applyManifest: %v", err)
	}

	assertApplied(t, dynamicClient, "", "namespaces", "fjord-test")
}

// TestApplyManifestIdempotent verifies re-applying the same manifest does
// not error and leaves the object's fields (here data.key) unchanged.
func TestApplyManifestIdempotent(t *testing.T) {
	t.Parallel()

	manifest := []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: fjord-config
  namespace: fjord-test
data:
  key: value
`)

	dynamicClient := newTestDynamicClient()
	mapper := testRESTMapper()

	for range 2 {
		if err := applyManifest(t.Context(), dynamicClient, mapper, manifest); err != nil {
			t.Fatalf("applyManifest: %v", err)
		}
	}

	obj := assertApplied(t, dynamicClient, "fjord-test", "configmaps", "fjord-config")

	data, found, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil || !found {
		t.Fatalf("NestedStringMap(data) = %v, %v, %v", data, found, err)
	}

	if got := data["key"]; got != "value" {
		t.Errorf("configmap data[key] = %q, want %q", got, "value")
	}
}

// TestApplyObjectUnresolvableKind verifies applyObject returns a wrapped
// error (rather than panicking or silently no-op'ing) when mapper cannot
// resolve obj's GroupVersionKind, e.g. because a manifest references a CRD
// not yet established.
func TestApplyObjectUnresolvableKind(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "DoesNotExist",
		"metadata":   map[string]any{"name": "x"},
	}}

	err := applyObject(t.Context(), newTestDynamicClient(), meta.NewDefaultRESTMapper(nil), obj)
	if err == nil {
		t.Fatal("applyObject() error = nil, want a REST mapping error")
	}
}

// assertApplied fetches gvr's namespace/name and fails the test if it does
// not exist, returning it for further assertions.
func assertApplied(t *testing.T, dynamicClient *dynamicfake.FakeDynamicClient, namespace, resource, name string) *unstructured.Unstructured {
	t.Helper()

	gvr := schema.GroupVersionResource{Version: "v1", Resource: resource}

	client := dynamicClient.Resource(gvr)

	var (
		obj *unstructured.Unstructured
		err error
	)

	if namespace == "" {
		obj, err = client.Get(t.Context(), name, metav1.GetOptions{})
	} else {
		obj, err = client.Namespace(namespace).Get(t.Context(), name, metav1.GetOptions{})
	}

	if err != nil {
		t.Fatalf("get %s %q: %v", resource, name, err)
	}

	return obj
}

// assertAppliedGVR fetches gvr's namespace/name and fails the test if it
// does not exist. It is assertApplied's sibling for non-core-v1 GVRs (which
// need an explicit Group).
func assertAppliedGVR(t *testing.T, dynamicClient *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, namespace, name string) {
	t.Helper()

	client := dynamicClient.Resource(gvr)

	var err error
	if namespace == "" {
		_, err = client.Get(t.Context(), name, metav1.GetOptions{})
	} else {
		_, err = client.Namespace(namespace).Get(t.Context(), name, metav1.GetOptions{})
	}

	if err != nil {
		t.Fatalf("get %s %q: %v", gvr.Resource, name, err)
	}
}
