package cluster

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/sivchari/fjord/internal/cluster/manifests"
)

func TestIPAddressPool(t *testing.T) {
	t.Parallel()

	obj := ipAddressPool("172.19.255.200-172.19.255.250")

	if got := obj.GetName(); got != metalLBPoolName {
		t.Errorf("name = %q, want %q", got, metalLBPoolName)
	}

	if got := obj.GetNamespace(); got != metalLBNamespace {
		t.Errorf("namespace = %q, want %q", got, metalLBNamespace)
	}

	addresses, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "addresses")
	if err != nil || !found {
		t.Fatalf("NestedStringSlice(spec.addresses) = %v, %v, %v", addresses, found, err)
	}

	const want = "172.19.255.200-172.19.255.250"
	if len(addresses) != 1 || addresses[0] != want {
		t.Errorf("spec.addresses = %v, want [%q]", addresses, want)
	}
}

func TestL2Advertisement(t *testing.T) {
	t.Parallel()

	obj := l2Advertisement()

	if got := obj.GetName(); got != metalLBPoolName {
		t.Errorf("name = %q, want %q", got, metalLBPoolName)
	}

	if got := obj.GetNamespace(); got != metalLBNamespace {
		t.Errorf("namespace = %q, want %q", got, metalLBNamespace)
	}

	pools, found, err := unstructured.NestedStringSlice(obj.Object, "spec", "ipAddressPools")
	if err != nil || !found {
		t.Fatalf("NestedStringSlice(spec.ipAddressPools) = %v, %v, %v", pools, found, err)
	}

	if len(pools) != 1 || pools[0] != metalLBPoolName {
		t.Errorf("spec.ipAddressPools = %v, want [%q]", pools, metalLBPoolName)
	}
}

// TestApplyManifestMetalLBNative is a regression test verifying the embedded
// metallb-native.yaml (manifests.MetalLBNative) decodes and applies cleanly
// end to end: every document's REST mapping resolves, and representative
// resources across every kind it defines land in the fake dynamic client.
// It uses a hand-built RESTMapper covering exactly the kinds the manifest is
// pinned to (see its header comment) rather than a live cluster's discovery.
func TestApplyManifestMetalLBNative(t *testing.T) {
	t.Parallel()

	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := metalLBNativeRESTMapper()

	if err := applyManifest(t.Context(), dynamicClient, mapper, manifests.MetalLBNative); err != nil {
		t.Fatalf("applyManifest(metallb-native.yaml): %v", err)
	}

	assertApplied(t, dynamicClient, "", "namespaces", metalLBNamespace)
	assertAppliedGVR(t, dynamicClient,
		schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
		"", "ipaddresspools.metallb.io")
	assertAppliedGVR(t, dynamicClient,
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		metalLBNamespace, metalLBControllerDeploymentName)
	assertAppliedGVR(t, dynamicClient,
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
		metalLBNamespace, "speaker")
	assertAppliedGVR(t, dynamicClient,
		schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"},
		"", metalLBWebhookConfigurationName)
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

// metalLBNativeRESTMapper returns a RESTMapper covering every
// Group/Version/Kind manifests.MetalLBNative (pinned to metallb v0.14.9)
// defines, standing in for a real cluster's discovery-derived RESTMapper.
func metalLBNativeRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)

	root := []schema.GroupVersionKind{
		{Version: "v1", Kind: "Namespace"},
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"},
	}
	for _, gvk := range root {
		mapper.Add(gvk, meta.RESTScopeRoot)
	}

	namespaced := []schema.GroupVersionKind{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		{Version: "v1", Kind: "ServiceAccount"},
		{Version: "v1", Kind: "ConfigMap"},
		{Version: "v1", Kind: "Secret"},
		{Version: "v1", Kind: "Service"},
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"},
	}
	for _, gvk := range namespaced {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}

	return mapper
}

// TestWaitForMetalLBWebhookReady verifies waitForMetalLBWebhookReady blocks
// while metalLBWebhookConfigurationName is missing or has an empty
// caBundle, and returns once every webhook's caBundle is populated --
// mirroring metallb's controller patching it in place at startup.
func TestWaitForMetalLBWebhookReady(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	ready, err := metalLBWebhookReady(t.Context(), client)
	if err != nil {
		t.Fatalf("metalLBWebhookReady() before creation: %v", err)
	}

	if ready {
		t.Fatal("metalLBWebhookReady() = true before the webhook configuration exists, want false")
	}

	notReady := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: metalLBWebhookConfigurationName},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{Name: "ipaddresspoolvalidationwebhook.metallb.io"},
		},
	}

	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(t.Context(), notReady, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create validating webhook configuration: %v", err)
	}

	assertMetalLBReady(t, client, false, "with an empty caBundle")

	notReady.Webhooks[0].ClientConfig.CABundle = []byte("fake-ca-bundle")

	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(t.Context(), notReady, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update validating webhook configuration: %v", err)
	}

	// The caBundle alone is not enough: the controller must also be Available
	// for its webhook endpoint to accept connections.
	assertMetalLBReady(t, client, false, "before the controller is Available")

	controller := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: metalLBControllerDeploymentName, Namespace: metalLBNamespace},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	if _, err := client.AppsV1().Deployments(metalLBNamespace).Create(t.Context(), controller, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create controller deployment: %v", err)
	}

	assertMetalLBReady(t, client, true, "with caBundle set and the controller Available")
}

// assertMetalLBReady checks metalLBWebhookReady returns want for client.
func assertMetalLBReady(t *testing.T, client *fake.Clientset, want bool, when string) {
	t.Helper()

	ready, err := metalLBWebhookReady(t.Context(), client)
	if err != nil {
		t.Fatalf("metalLBWebhookReady() %s: %v", when, err)
	}

	if ready != want {
		t.Fatalf("metalLBWebhookReady() = %v %s, want %v", ready, when, want)
	}
}
