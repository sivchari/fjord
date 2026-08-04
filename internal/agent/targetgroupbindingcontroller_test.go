package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

// discardLogger is a *slog.Logger that drops every record, keeping test
// output free of the warnings/debug lines TargetGroupBindingController logs
// as part of its normal, expected behavior.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// tgbUnstructured builds an unstructured TargetGroupBinding for use in a
// test's tgbIndexer or dynamic fake client, without going through
// decodeTargetGroupBinding.
func tgbUnstructured(namespace, name string, generation int64, serviceName string, port intstr.IntOrString, targetType string) *unstructured.Unstructured {
	spec := map[string]any{
		"serviceRef": map[string]any{
			"name": serviceName,
			"port": intOrStringToUnstructured(port),
		},
	}

	if targetType != "" {
		spec["targetType"] = targetType
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": TargetGroupBindingGroup + "/" + TargetGroupBindingVersion,
		"kind":       TargetGroupBindingKind,
		"metadata": map[string]any{
			"name":       name,
			"namespace":  namespace,
			"generation": generation,
		},
		"spec": spec,
	}}
}

// intOrStringToUnstructured returns the JSON-equivalent value
// intstr.IntOrString marshals to, since unstructured.Unstructured.Object
// entries must be plain (string/int64/bool/map/slice) values, not typed
// Go structs.
func intOrStringToUnstructured(v intstr.IntOrString) any {
	if v.Type == intstr.String {
		return v.StrVal
	}

	return int64(v.IntVal)
}

// newTGBIndexer returns a cache.Indexer (as TargetGroupBindingController's
// informer would build) pre-populated with objs.
func newTGBIndexer(t *testing.T, objs ...*unstructured.Unstructured) cache.Indexer {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	for _, obj := range objs {
		if err := indexer.Add(obj); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}

	return indexer
}

// newServiceLister returns a corelisters.ServiceLister (as
// TargetGroupBindingController's Service informer would build)
// pre-populated with objs.
func newServiceLister(t *testing.T, objs ...*corev1.Service) corelisters.ServiceLister {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	for _, obj := range objs {
		if err := indexer.Add(obj); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}

	return corelisters.NewServiceLister(indexer)
}

// newEndpointSliceLister returns a discoverylisters.EndpointSliceLister (as
// TargetGroupBindingController's EndpointSlice informer would build)
// pre-populated with objs.
func newEndpointSliceLister(t *testing.T, objs ...*discoveryv1.EndpointSlice) discoverylisters.EndpointSliceLister {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	for _, obj := range objs {
		if err := indexer.Add(obj); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}

	return discoverylisters.NewEndpointSliceLister(indexer)
}

// newNodeLister returns a corelisters.NodeLister (as
// TargetGroupBindingController's Node informer would build) pre-populated
// with objs.
func newNodeLister(t *testing.T, objs ...*corev1.Node) corelisters.NodeLister {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	for _, obj := range objs {
		if err := indexer.Add(obj); err != nil {
			t.Fatalf("indexer.Add: %v", err)
		}
	}

	return corelisters.NewNodeLister(indexer)
}

// newTestController returns a TargetGroupBindingController wired to a fake
// Kubernetes clientset seeded with kubeObjs and a fake dynamic client seeded
// with dynamicObjs (typically the TargetGroupBinding(s) reconcile writes
// status back to).
func newTestController(dynamicObjs []runtime.Object, kubeObjs ...runtime.Object) (*TargetGroupBindingController, *dynamicfake.FakeDynamicClient) {
	client := fake.NewClientset(kubeObjs...)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, dynamicObjs...)

	return NewTargetGroupBindingController(client, dynamicClient, discardLogger()), dynamicClient
}

func TestReconcileWritesIPTargetsStatus(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abcde", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		Ports:      []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(int32(8080))}},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.244.0.6"}},
			{Addresses: []string{"10.244.0.5"}},
		},
	}
	tgb := tgbUnstructured("default", "web", 3, "web", intstr.FromInt32(80), "")

	c, dynamicClient := newTestController([]runtime.Object{tgb}, service)

	err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t, service), newEndpointSliceLister(t, slice), newNodeLister(t))
	if err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	status := getTGBStatus(t, dynamicClient, "default", "web")

	assertNestedInt64(t, status, 3, "observedGeneration")
	assertNestedString(t, status, "ip", "targetType")

	targets, found, err := unstructured.NestedSlice(status, "targets")
	if err != nil || !found {
		t.Fatalf("status.targets = %v, %v, %v", targets, found, err)
	}

	if len(targets) != 2 {
		t.Fatalf("status.targets = %v, want 2 entries", targets)
	}

	assertTarget(t, targets[0], "10.244.0.5", 8080)
	assertTarget(t, targets[1], "10.244.0.6", 8080)
}

func TestReconcileWritesInstanceTargetsStatus(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 31080}},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.10"}}},
	}
	tgb := tgbUnstructured("default", "web", 1, "web", intstr.FromInt32(80), targetTypeInstance)

	c, dynamicClient := newTestController([]runtime.Object{tgb}, service)

	err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t, service), newEndpointSliceLister(t), newNodeLister(t, node))
	if err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	status := getTGBStatus(t, dynamicClient, "default", "web")

	assertNestedString(t, status, "instance", "targetType")

	targets, found, err := unstructured.NestedSlice(status, "targets")
	if err != nil || !found || len(targets) != 1 {
		t.Fatalf("status.targets = %v, %v, %v, want exactly 1 entry", targets, found, err)
	}

	assertTarget(t, targets[0], "192.168.1.10", 31080)
}

func TestReconcileInstanceAgainstClusterIPServiceReturnsError(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
	tgb := tgbUnstructured("default", "web", 1, "web", intstr.FromInt32(80), targetTypeInstance)

	c, dynamicClient := newTestController([]runtime.Object{tgb}, service)

	// reconcile logs the error and returns nil (it is a permanent
	// misconfiguration, not a transient failure worth requeuing), so we
	// assert on the absence of a status update instead of a returned error.
	if err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t, service), newEndpointSliceLister(t), newNodeLister(t)); err != nil {
		t.Fatalf("reconcile() error = %v, want nil (logged and skipped)", err)
	}

	obj, err := dynamicClient.Resource(TargetGroupBindingGVR).Namespace("default").Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get target group binding: %v", err)
	}

	if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
		t.Errorf("status = %v, want unset", obj.Object["status"])
	}
}

func TestReconcileSkipsSelectorlessServiceWithoutError(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "headless", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	tgb := tgbUnstructured("default", "headless", 1, "headless", intstr.FromInt32(80), "")

	c, dynamicClient := newTestController([]runtime.Object{tgb}, service)

	if err := c.reconcile(t.Context(), "default/headless", newTGBIndexer(t, tgb), newServiceLister(t, service), newEndpointSliceLister(t), newNodeLister(t)); err != nil {
		t.Fatalf("reconcile() error = %v, want nil (skip with a warning)", err)
	}

	obj, err := dynamicClient.Resource(TargetGroupBindingGVR).Namespace("default").Get(t.Context(), "headless", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get target group binding: %v", err)
	}

	if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
		t.Errorf("status = %v, want unset", obj.Object["status"])
	}
}

func TestReconcileReferencedServiceNotFound(t *testing.T) {
	t.Parallel()

	tgb := tgbUnstructured("default", "web", 1, "does-not-exist", intstr.FromInt32(80), "")

	c, _ := newTestController([]runtime.Object{tgb})

	err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t), newEndpointSliceLister(t), newNodeLister(t))
	if err == nil {
		t.Fatal("reconcile() error = nil, want an error for a missing referenced service")
	}
}

func TestReconcileDoesNothingWhenTargetGroupBindingGone(t *testing.T) {
	t.Parallel()

	c, _ := newTestController(nil)

	// The indexer carries no TargetGroupBinding for "default/web", as if it
	// had just been deleted. reconcile has nothing left to clean up (see
	// its doc comment), so this must be a no-op, not an error.
	if err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t), newServiceLister(t), newEndpointSliceLister(t), newNodeLister(t)); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}
}

// TestReconcileIsNoOpWhenTargetsUnchanged verifies a second reconcile that
// resolves the exact same targets as the first does not write status again
// (see updateStatus's doc comment) -- observed via the TargetGroupBinding's
// resourceVersion staying the same across both calls.
func TestReconcileIsNoOpWhenTargetsUnchanged(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abcde", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		Ports:      []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(int32(8080))}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.244.0.5"}}},
	}
	tgb := tgbUnstructured("default", "web", 1, "web", intstr.FromInt32(80), "")

	c, dynamicClient := newTestController([]runtime.Object{tgb}, service)

	indexer := newTGBIndexer(t, tgb)
	serviceLister := newServiceLister(t, service)
	endpointSliceLister := newEndpointSliceLister(t, slice)
	nodeLister := newNodeLister(t)

	if err := c.reconcile(t.Context(), "default/web", indexer, serviceLister, endpointSliceLister, nodeLister); err != nil {
		t.Fatalf("reconcile() #1 error: %v", err)
	}

	afterFirst, err := dynamicClient.Resource(TargetGroupBindingGVR).Namespace("default").Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get target group binding after first reconcile: %v", err)
	}

	// The controller's real informer indexer would already reflect
	// afterFirst's resourceVersion (the informer watches the same object it
	// reconciles), so refresh the test's indexer too before reconciling
	// again.
	if err := indexer.Update(afterFirst); err != nil {
		t.Fatalf("indexer.Update: %v", err)
	}

	if err := c.reconcile(t.Context(), "default/web", indexer, serviceLister, endpointSliceLister, nodeLister); err != nil {
		t.Fatalf("reconcile() #2 error: %v", err)
	}

	afterSecond, err := dynamicClient.Resource(TargetGroupBindingGVR).Namespace("default").Get(t.Context(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get target group binding after second reconcile: %v", err)
	}

	if afterFirst.GetResourceVersion() != afterSecond.GetResourceVersion() {
		t.Errorf("resourceVersion changed from %q to %q on a no-op reconcile", afterFirst.GetResourceVersion(), afterSecond.GetResourceVersion())
	}
}

func TestEnqueueTargetGroupBindingsForService(t *testing.T) {
	t.Parallel()

	matching := tgbUnstructured("default", "web", 1, "web", intstr.FromInt32(80), "")
	otherService := tgbUnstructured("default", "other", 1, "other-svc", intstr.FromInt32(80), "")
	otherNamespace := tgbUnstructured("kube-system", "web", 1, "web", intstr.FromInt32(80), "")

	indexer := newTGBIndexer(t, matching, otherService, otherNamespace)
	queue := newTestQueue()
	c, _ := newTestController(nil)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	c.enqueueTargetGroupBindingsForService(queue, indexer, svc)

	if got, want := queue.Len(), 1; got != want {
		t.Fatalf("queue.Len() = %d, want %d (items: %v)", got, want, queue.items)
	}

	if queue.items[0] != "default/web" {
		t.Errorf("enqueued key = %q, want %q", queue.items[0], "default/web")
	}
}

func TestEnqueueTargetGroupBindingsForServiceIgnoresNonService(t *testing.T) {
	t.Parallel()

	c, _ := newTestController(nil)
	queue := newTestQueue()

	c.enqueueTargetGroupBindingsForService(queue, newTGBIndexer(t), "not-a-service")

	if got := queue.Len(); got != 0 {
		t.Errorf("queue.Len() = %d, want 0", got)
	}
}

func TestEnqueueTargetGroupBindingsForEndpointSlice(t *testing.T) {
	t.Parallel()

	matching := tgbUnstructured("default", "web", 1, "web", intstr.FromInt32(80), "")
	other := tgbUnstructured("default", "other", 1, "other-svc", intstr.FromInt32(80), "")

	indexer := newTGBIndexer(t, matching, other)
	queue := newTestQueue()
	c, _ := newTestController(nil)

	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abcde", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
	}
	c.enqueueTargetGroupBindingsForEndpointSlice(queue, indexer, slice)

	if got, want := queue.Len(), 1; got != want {
		t.Fatalf("queue.Len() = %d, want %d (items: %v)", got, want, queue.items)
	}

	if queue.items[0] != "default/web" {
		t.Errorf("enqueued key = %q, want %q", queue.items[0], "default/web")
	}
}

func TestEnqueueTargetGroupBindingsForEndpointSliceIgnoresNonEndpointSlice(t *testing.T) {
	t.Parallel()

	c, _ := newTestController(nil)
	queue := newTestQueue()

	c.enqueueTargetGroupBindingsForEndpointSlice(queue, newTGBIndexer(t), "not-an-endpoint-slice")

	if got := queue.Len(); got != 0 {
		t.Errorf("queue.Len() = %d, want 0", got)
	}
}

func TestEnqueueAllTargetGroupBindings(t *testing.T) {
	t.Parallel()

	a := tgbUnstructured("default", "a", 1, "a", intstr.FromInt32(80), "")
	b := tgbUnstructured("kube-system", "b", 1, "b", intstr.FromInt32(80), "")

	indexer := newTGBIndexer(t, a, b)
	queue := newTestQueue()

	enqueueAllTargetGroupBindings(queue, indexer)

	if got, want := queue.Len(), 2; got != want {
		t.Fatalf("queue.Len() = %d, want %d (items: %v)", got, want, queue.items)
	}
}

// TestGarbageCollectLegacyMirrorServicesDeletesLegacyMirrors verifies
// garbageCollectLegacyMirrorServices deletes a Service matching every trait
// an earlier fjord version's mirror-Service emulation gave one, and leaves
// alone a same-named Service in a different namespace that lacks fjord's
// managed-by label entirely.
func TestGarbageCollectLegacyMirrorServicesDeletesLegacyMirrors(t *testing.T) {
	t.Parallel()

	legacyMirror := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-fjord-tgb",
			Namespace: "default",
			Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: TargetGroupBindingGroup + "/" + TargetGroupBindingVersion,
				Kind:       TargetGroupBindingKind,
				Name:       "web",
				UID:        types.UID("11111111-1111-1111-1111-111111111111"),
			}},
		},
	}
	foreignUnlabeled := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web-fjord-tgb", Namespace: "other-ns"},
	}

	c, _ := newTestController(nil, legacyMirror, foreignUnlabeled)

	if err := c.garbageCollectLegacyMirrorServices(t.Context()); err != nil {
		t.Fatalf("garbageCollectLegacyMirrorServices() error: %v", err)
	}

	_, err := c.client.CoreV1().Services("default").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("get legacy mirror after gc: err = %v, want NotFound", err)
	}

	if _, err := c.client.CoreV1().Services("other-ns").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{}); err != nil {
		t.Errorf("unlabeled same-named service was deleted: %v", err)
	}
}

// TestGarbageCollectLegacyMirrorServicesIsSafeWithoutLegacyMirrors verifies
// garbageCollectLegacyMirrorServices does not error when no fjord-managed
// Service exists at all.
func TestGarbageCollectLegacyMirrorServicesIsSafeWithoutLegacyMirrors(t *testing.T) {
	t.Parallel()

	c, _ := newTestController(nil)

	if err := c.garbageCollectLegacyMirrorServices(t.Context()); err != nil {
		t.Fatalf("garbageCollectLegacyMirrorServices() error: %v", err)
	}
}

// TestTargetGroupBindingControllerRunReturnsOnContextCancel verifies Run
// returns promptly, without hanging, when handed an already-done context --
// the shape a caller (cmd/fjord-agent) relies on to shut the controller
// down without leaking its goroutines even if cancellation races its own
// startup. An already-done context can race the informers' first list, so
// Run may legitimately report a wrapped context.Canceled cache-sync
// failure rather than nil; either is an acceptable, prompt exit.
func TestTargetGroupBindingControllerRunReturnsOnContextCancel(t *testing.T) {
	t.Parallel()

	c, _ := newTestController(nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := c.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want nil or a wrapped context.Canceled", err)
	}
}

// TestTargetGroupBindingControllerRunReconciles is an end-to-end test of
// Run's full wiring (informers, event handlers, workers): starting from a
// pre-existing TargetGroupBinding, Service, and EndpointSlice, it verifies
// status.targets eventually reflects the EndpointSlice's Ready addresses,
// then that a new Ready pod address eventually propagates too.
func TestTargetGroupBindingControllerRunReconciles(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abcde", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		Ports:      []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(int32(8080))}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.244.0.5"}}},
	}
	tgb := tgbUnstructured("default", "web", 1, "web", intstr.FromInt32(80), "")

	client := fake.NewClientset(service, slice)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, tgb)
	c := NewTargetGroupBindingController(client, dynamicClient, discardLogger())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runDone := make(chan error, 1)

	go func() { runDone <- c.Run(ctx) }()

	waitForTargets(t, dynamicClient, []string{"10.244.0.5"})

	updated := slice.DeepCopy()
	updated.Endpoints = append(updated.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.244.0.6"}})

	if _, err := client.DiscoveryV1().EndpointSlices("default").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update endpoint slice: %v", err)
	}

	waitForTargets(t, dynamicClient, []string{"10.244.0.5", "10.244.0.6"})

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}
}

// waitForTargets polls until the "default/web" TargetGroupBinding's
// status.targets addresses equal wantAddrs, or fails the test after a
// bounded amount of waiting.
func waitForTargets(t *testing.T, dynamicClient *dynamicfake.FakeDynamicClient, wantAddrs []string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		obj, err := dynamicClient.Resource(TargetGroupBindingGVR).Namespace("default").Get(ctx, "web", metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get target group binding: %w", err)
		}

		targets, found, err := unstructured.NestedSlice(obj.Object, "status", "targets")
		if err != nil {
			return false, fmt.Errorf("read status.targets: %w", err)
		}

		if !found || len(targets) != len(wantAddrs) {
			return false, nil
		}

		for i, addr := range wantAddrs {
			m, ok := targets[i].(map[string]any)
			if !ok || m["address"] != addr {
				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		t.Fatalf("wait for status.targets %v: %v", wantAddrs, err)
	}
}

// getTGBStatus fetches the TargetGroupBinding named namespace/name through
// dynamicClient and returns its decoded status map, failing the test if it
// is missing.
func getTGBStatus(t *testing.T, dynamicClient *dynamicfake.FakeDynamicClient, namespace, name string) map[string]any {
	t.Helper()

	obj, err := dynamicClient.Resource(TargetGroupBindingGVR).Namespace(namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get target group binding %s/%s: %v", namespace, name, err)
	}

	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		t.Fatalf("status = %v, %v, %v, want present", status, found, err)
	}

	return status
}

// assertTarget fails the test unless got (a status.targets[i] entry decoded
// as map[string]any) has the given address and port.
func assertTarget(t *testing.T, got any, wantAddress string, wantPort int64) {
	t.Helper()

	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("target entry type = %T, want map[string]any", got)
	}

	if m["address"] != wantAddress {
		t.Errorf("target.address = %v, want %q", m["address"], wantAddress)
	}

	if m["port"] != wantPort {
		t.Errorf("target.port = %v, want %d", m["port"], wantPort)
	}
}

// assertNestedInt64 fails the test unless the int64 field at path in obj
// equals want.
func assertNestedInt64(t *testing.T, obj map[string]any, want int64, path ...string) {
	t.Helper()

	got, found, err := unstructured.NestedInt64(obj, path...)
	if err != nil || !found || got != want {
		t.Errorf("%v = %v, %v, %v, want %d", path, got, found, err, want)
	}
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

// testQueue is a minimal workqueue.TypedRateLimitingInterface[string] stub
// recording every key Add is called with, for tests that only exercise
// enqueue-side logic and never drain the queue.
type testQueue struct {
	items []string
}

func newTestQueue() *testQueue { return &testQueue{} }

func (q *testQueue) Add(item string)                    { q.items = append(q.items, item) }
func (q *testQueue) Len() int                           { return len(q.items) }
func (q *testQueue) Get() (item string, shutdown bool)  { return "", true }
func (q *testQueue) Done(_ string)                      {}
func (q *testQueue) ShutDown()                          {}
func (q *testQueue) ShutDownWithDrain()                 {}
func (q *testQueue) ShuttingDown() bool                 { return false }
func (q *testQueue) AddAfter(_ string, _ time.Duration) {}
func (q *testQueue) AddRateLimited(item string)         { q.items = append(q.items, item) }
func (q *testQueue) Forget(_ string)                    {}
func (q *testQueue) NumRequeues(_ string) int           { return 0 }
