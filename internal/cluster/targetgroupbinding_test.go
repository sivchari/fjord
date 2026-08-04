package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
)

// apiextensionsRESTMapper returns a RESTMapper resolving
// apiextensions.k8s.io/v1 CustomResourceDefinition -- a built-in Kubernetes
// API type present on any real cluster's discovery, unlike metallb's own
// CRDs (see loadbalancer_test.go's metalLBNativeRESTMapper), so
// EnsureTargetGroupBindingCRD never needs to rebuild its mapper after
// applying.
func apiextensionsRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, meta.RESTScopeRoot)

	return mapper
}

func TestTargetGroupBindingCRDObjectMetadata(t *testing.T) {
	t.Parallel()

	obj := mustTargetGroupBindingCRDObject(t)

	if got, want := obj.GetAPIVersion(), "apiextensions.k8s.io/v1"; got != want {
		t.Errorf("apiVersion = %q, want %q", got, want)
	}

	if got, want := obj.GetKind(), "CustomResourceDefinition"; got != want {
		t.Errorf("kind = %q, want %q", got, want)
	}

	if got, want := obj.GetName(), "targetgroupbindings.elbv2.k8s.aws"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	assertNestedString(t, obj.Object, "elbv2.k8s.aws", "spec", "group")
	assertNestedString(t, obj.Object, "Namespaced", "spec", "scope")
	assertNestedString(t, obj.Object, "TargetGroupBinding", "spec", "names", "kind")
	assertNestedString(t, obj.Object, "targetgroupbindings", "spec", "names", "plural")
}

func TestTargetGroupBindingCRDObjectVersion(t *testing.T) {
	t.Parallel()

	obj := mustTargetGroupBindingCRDObject(t)

	versions, found, err := unstructured.NestedSlice(obj.Object, "spec", "versions")
	if err != nil || !found || len(versions) != 1 {
		t.Fatalf("spec.versions = %v, %v, %v, want exactly 1 entry", versions, found, err)
	}

	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("spec.versions[0] type = %T, want map[string]any", versions[0])
	}

	assertNestedString(t, version, "v1beta1", "name")
	assertNestedTrue(t, version, "served")
	assertNestedTrue(t, version, "storage")

	statusSubresource, found, err := unstructured.NestedMap(version, "subresources", "status")
	if err != nil || !found || statusSubresource == nil {
		t.Errorf("spec.versions[0].subresources.status = %v, %v, %v, want present", statusSubresource, found, err)
	}
}

func TestTargetGroupBindingCRDObjectSpecSchema(t *testing.T) {
	t.Parallel()

	obj := mustTargetGroupBindingCRDObject(t)

	versions, _, err := unstructured.NestedSlice(obj.Object, "spec", "versions")
	if err != nil || len(versions) != 1 {
		t.Fatalf("spec.versions: %v, %v", versions, err)
	}

	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("spec.versions[0] type = %T, want map[string]any", versions[0])
	}

	specRequired, found, err := unstructured.NestedStringSlice(version, "schema", "openAPIV3Schema", "properties", "spec", "required")
	if err != nil || !found {
		t.Fatalf("spec schema required = %v, %v, %v", specRequired, found, err)
	}

	assertContains(t, specRequired, "targetGroupARN")
	assertContains(t, specRequired, "serviceRef")

	assertNestedTrue(t, version, "schema", "openAPIV3Schema", "properties", "spec", "x-kubernetes-preserve-unknown-fields")

	portFields := []string{"schema", "openAPIV3Schema", "properties", "spec", "properties", "serviceRef", "properties", "port"}
	assertNestedTrue(t, version, append(portFields, "x-kubernetes-int-or-string")...)

	portType, found, _ := unstructured.NestedString(version, append(portFields, "type")...)
	if found || portType != "" {
		t.Errorf("serviceRef.port type = %q (found=%v), want unset (only x-kubernetes-int-or-string)", portType, found)
	}
}

func TestTargetGroupBindingCRDObjectStatusSchema(t *testing.T) {
	t.Parallel()

	obj := mustTargetGroupBindingCRDObject(t)

	versions, _, err := unstructured.NestedSlice(obj.Object, "spec", "versions")
	if err != nil || len(versions) != 1 {
		t.Fatalf("spec.versions: %v, %v", versions, err)
	}

	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("spec.versions[0] type = %T, want map[string]any", versions[0])
	}

	assertNestedTrue(t, version, statusFieldPath("x-kubernetes-preserve-unknown-fields")...)
	assertNestedString(t, version, "integer", statusFieldPath("properties", "observedGeneration", "type")...)
	assertNestedString(t, version, "array", statusFieldPath("properties", "targets", "type")...)
	assertNestedString(t, version, "string", statusFieldPath("properties", "targets", "items", "properties", "address", "type")...)
	assertNestedString(t, version, "integer", statusFieldPath("properties", "targets", "items", "properties", "port", "type")...)

	targetTypeEnum, found, err := unstructured.NestedSlice(version, statusFieldPath("properties", "targetType", "enum")...)
	if err != nil || !found || len(targetTypeEnum) != 2 {
		t.Fatalf("status.targetType.enum = %v, %v, %v, want exactly 2 entries", targetTypeEnum, found, err)
	}
}

// statusFieldPath returns the openAPIV3Schema path to TargetGroupBinding's
// status property, with more appended -- freshly allocated on every call so
// callers can never alias each other's slice.
func statusFieldPath(more ...string) []string {
	return append([]string{"schema", "openAPIV3Schema", "properties", "status"}, more...)
}

// mustTargetGroupBindingCRDObject builds the CRD object, failing the test on
// error.
func mustTargetGroupBindingCRDObject(t *testing.T) *unstructured.Unstructured {
	t.Helper()

	obj, err := targetGroupBindingCRDObject()
	if err != nil {
		t.Fatalf("targetGroupBindingCRDObject() error: %v", err)
	}

	return obj
}

// assertNestedString fails the test unless the string field at path in obj
// equals want.
func assertNestedString(t *testing.T, obj map[string]any, want string, path ...string) {
	t.Helper()

	got, found, err := unstructured.NestedString(obj, path...)
	if err != nil || !found || got != want {
		t.Errorf("%v = %v, %v, %v, want %q", path, got, found, err, want)
	}
}

// assertNestedTrue fails the test unless the bool field at path in obj is
// true.
func assertNestedTrue(t *testing.T, obj map[string]any, path ...string) {
	t.Helper()

	got, found, err := unstructured.NestedBool(obj, path...)
	if err != nil || !found || !got {
		t.Errorf("%v = %v, %v, %v, want true", path, got, found, err)
	}
}

// assertContains fails the test if want is not present in got.
func assertContains(t *testing.T, got []string, want string) {
	t.Helper()

	for _, v := range got {
		if v == want {
			return
		}
	}

	t.Errorf("%v does not contain %q", got, want)
}

// TestEnsureTargetGroupBindingCRDIdempotent verifies EnsureTargetGroupBindingCRD
// creates the CRD on a first call and does not error on a second, matching
// every other EnsureXxx function's idempotency contract.
func TestEnsureTargetGroupBindingCRDIdempotent(t *testing.T) {
	t.Parallel()

	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := apiextensionsRESTMapper()
	obj := mustTargetGroupBindingCRDObject(t)

	for range 2 {
		if err := applyObject(t.Context(), dynamicClient, mapper, obj); err != nil {
			t.Fatalf("applyObject: %v", err)
		}
	}

	gvr := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	assertAppliedGVR(t, dynamicClient, gvr, "", "targetgroupbindings.elbv2.k8s.aws")
}
