package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// discardLogger is a *slog.Logger that drops every record, keeping test
// output free of the warnings/debug lines TargetGroupBindingController logs
// as part of its normal, expected behavior.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// tgbUnstructured builds an unstructured TargetGroupBinding for use in a
// test's tgbIndexer, without going through decodeTargetGroupBinding.
func tgbUnstructured(namespace, name string, uid types.UID, serviceName string, port intstr.IntOrString, targetGroupARN string) *unstructured.Unstructured {
	spec := map[string]any{
		"serviceRef": map[string]any{
			"name": serviceName,
			"port": intOrStringToUnstructured(port),
		},
	}

	if targetGroupARN != "" {
		spec["targetGroupARN"] = targetGroupARN
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": TargetGroupBindingGroup + "/" + TargetGroupBindingVersion,
		"kind":       TargetGroupBindingKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"uid":       string(uid),
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

func TestReconcileCreatesMirrorService(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	uid := types.UID("11111111-1111-1111-1111-111111111111")
	tgb := tgbUnstructured("default", "web", uid, "web", intstr.FromInt32(80), "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/web/abc123")

	client := fake.NewClientset(service)
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t, service))
	if err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	mirror, err := client.CoreV1().Services("default").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get mirror service: %v", err)
	}

	if mirror.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("mirror Spec.Type = %v, want %v", mirror.Spec.Type, corev1.ServiceTypeLoadBalancer)
	}

	if mirror.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("mirror Labels[%s] = %q, want %q", managedByLabelKey, mirror.Labels[managedByLabelKey], managedByLabelValue)
	}

	if len(mirror.OwnerReferences) != 1 || mirror.OwnerReferences[0].UID != uid {
		t.Errorf("mirror OwnerReferences = %v, want a single owner with UID %s", mirror.OwnerReferences, uid)
	}
}

func TestReconcileUpdatesExistingMirrorServicePreservingNodePort(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web", "version": "v2"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(9090)}},
		},
	}
	existingMirror := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-fjord-tgb",
			Namespace: "default",
			Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080), NodePort: 31234}},
		},
	}
	tgb := tgbUnstructured("default", "web", "", "web", intstr.FromInt32(80), "")

	client := fake.NewClientset(service, existingMirror)
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	if err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t, service)); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	mirror, err := client.CoreV1().Services("default").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get mirror service: %v", err)
	}

	if len(mirror.Spec.Selector) != 2 || mirror.Spec.Selector["version"] != "v2" {
		t.Errorf("mirror Spec.Selector = %v, want updated to %v", mirror.Spec.Selector, service.Spec.Selector)
	}

	if len(mirror.Spec.Ports) != 1 {
		t.Fatalf("mirror Spec.Ports = %v, want exactly 1", mirror.Spec.Ports)
	}

	if mirror.Spec.Ports[0].TargetPort != intstr.FromInt32(9090) {
		t.Errorf("mirror Spec.Ports[0].TargetPort = %v, want updated to 9090", mirror.Spec.Ports[0].TargetPort)
	}

	if mirror.Spec.Ports[0].NodePort != 31234 {
		t.Errorf("mirror Spec.Ports[0].NodePort = %d, want preserved 31234", mirror.Spec.Ports[0].NodePort)
	}
}

func TestReconcileRefusesForeignService(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80}},
		},
	}
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web-fjord-tgb", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 1234}}},
	}
	tgb := tgbUnstructured("default", "web", "", "web", intstr.FromInt32(80), "")

	client := fake.NewClientset(service, foreign)
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t, service))
	if err == nil {
		t.Fatal("reconcile() error = nil, want an error for a same-named Service fjord does not manage")
	}

	unchanged, getErr := client.CoreV1().Services("default").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("get foreign service: %v", getErr)
	}

	if unchanged.Spec.Ports[0].Port != 1234 {
		t.Errorf("foreign service was modified: %+v", unchanged.Spec.Ports)
	}
}

func TestReconcileDeletesMirrorWhenTargetGroupBindingGone(t *testing.T) {
	t.Parallel()

	managedMirror := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-fjord-tgb",
			Namespace: "default",
			Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
		},
	}

	client := fake.NewClientset(managedMirror)
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	// The indexer carries no TargetGroupBinding for "default/web", as if it
	// had just been deleted.
	if err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t), newServiceLister(t)); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	_, err := client.CoreV1().Services("default").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get mirror service after reconcile: err = %v, want NotFound", err)
	}
}

func TestReconcileDoesNotDeleteForeignServiceWhenTargetGroupBindingGone(t *testing.T) {
	t.Parallel()

	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web-fjord-tgb", Namespace: "default"},
	}

	client := fake.NewClientset(foreign)
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	if err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t), newServiceLister(t)); err != nil {
		t.Fatalf("reconcile() error: %v", err)
	}

	if _, err := client.CoreV1().Services("default").Get(t.Context(), "web-fjord-tgb", metav1.GetOptions{}); err != nil {
		t.Fatalf("foreign service was deleted: %v", err)
	}
}

func TestReconcileSkipsSelectorlessServiceWithoutError(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "headless", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	tgb := tgbUnstructured("default", "headless", "", "headless", intstr.FromInt32(80), "")

	client := fake.NewClientset(service)
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	if err := c.reconcile(t.Context(), "default/headless", newTGBIndexer(t, tgb), newServiceLister(t, service)); err != nil {
		t.Fatalf("reconcile() error = %v, want nil (skip with a warning)", err)
	}

	_, err := client.CoreV1().Services("default").Get(t.Context(), "headless-fjord-tgb", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("mirror service should not have been created, get err = %v", err)
	}
}

func TestReconcileReferencedServiceNotFound(t *testing.T) {
	t.Parallel()

	tgb := tgbUnstructured("default", "web", "", "does-not-exist", intstr.FromInt32(80), "")

	client := fake.NewClientset()
	c := NewTargetGroupBindingController(client, dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	err := c.reconcile(t.Context(), "default/web", newTGBIndexer(t, tgb), newServiceLister(t))
	if err == nil {
		t.Fatal("reconcile() error = nil, want an error for a missing referenced service")
	}
}

func TestEnqueueTargetGroupBindingsForService(t *testing.T) {
	t.Parallel()

	matching := tgbUnstructured("default", "web", "", "web", intstr.FromInt32(80), "")
	otherService := tgbUnstructured("default", "other", "", "other-svc", intstr.FromInt32(80), "")
	otherNamespace := tgbUnstructured("kube-system", "web", "", "web", intstr.FromInt32(80), "")

	indexer := newTGBIndexer(t, matching, otherService, otherNamespace)
	queue := newTestQueue()
	c := NewTargetGroupBindingController(fake.NewClientset(), dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

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

	c := NewTargetGroupBindingController(fake.NewClientset(), dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())
	queue := newTestQueue()

	c.enqueueTargetGroupBindingsForService(queue, newTGBIndexer(t), "not-a-service")

	if got := queue.Len(); got != 0 {
		t.Errorf("queue.Len() = %d, want 0", got)
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

	c := NewTargetGroupBindingController(fake.NewClientset(), dynamicfake.NewSimpleDynamicClient(scheme.Scheme), discardLogger())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := c.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want nil or a wrapped context.Canceled", err)
	}
}

// TestTargetGroupBindingControllerRunReconciles is an end-to-end test of
// Run's full wiring (informers, event handlers, workers): starting from a
// pre-existing TargetGroupBinding and Service, it verifies the mirror
// Service eventually appears, then that updating the Service's selector
// eventually propagates to the mirror.
func TestTargetGroupBindingControllerRunReconciles(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	tgb := tgbUnstructured("default", "web", "", "web", intstr.FromInt32(80), "")

	client := fake.NewClientset(service)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, tgb)
	c := NewTargetGroupBindingController(client, dynamicClient, discardLogger())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runDone := make(chan error, 1)

	go func() { runDone <- c.Run(ctx) }()

	waitForMirrorSelector(t, client, map[string]string{"app": "web"})

	updated := service.DeepCopy()
	updated.Spec.Selector = map[string]string{"app": "web", "canary": "true"}

	if _, err := client.CoreV1().Services("default").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update service: %v", err)
	}

	waitForMirrorSelector(t, client, map[string]string{"app": "web", "canary": "true"})

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

// waitForMirrorSelector polls until the "web-fjord-tgb" mirror Service's
// selector equals want, or fails the test after a bounded amount of
// waiting.
func waitForMirrorSelector(t *testing.T, client *fake.Clientset, want map[string]string) {
	t.Helper()

	err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		mirror, err := client.CoreV1().Services("default").Get(ctx, "web-fjord-tgb", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("get mirror service: %w", err)
		}

		if len(mirror.Spec.Selector) != len(want) {
			return false, nil
		}

		for k, v := range want {
			if mirror.Spec.Selector[k] != v {
				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		t.Fatalf("wait for mirror service selector %v: %v", want, err)
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
