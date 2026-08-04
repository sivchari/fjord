package agent

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// targetGroupBindingWorkers is the number of reconcile goroutines
// TargetGroupBindingController.Run starts.
const targetGroupBindingWorkers = 2

// TargetGroupBindingController emulates the AWS Load Balancer Controller's
// TargetGroupBinding (elbv2.k8s.aws/v1beta1) CRD locally: fjord has no ALB
// to register pod/node targets with, so for every TargetGroupBinding it
// instead resolves the targets a real ALB would register -- pod IPs for
// targetType ip, node address + nodePort for targetType instance (see
// resolveTargets) -- from the Service it references, and reports them in
// the TargetGroupBinding's own status (see updateStatus), rather than
// mutating the referenced Service or creating one of its own.
//
// It watches TargetGroupBinding (via a dynamic informer, since fjord has no
// generated client for it), Service, EndpointSlice, and Node, so any change
// that could affect a TargetGroupBinding's resolved targets -- a
// serviceRef change, a pod becoming ready/unready, a node's address
// changing -- is picked up, reconciling affected TargetGroupBindings
// through a rate-limited work queue. Call Run to start it; Run blocks until
// ctx is done, so callers should run it in its own goroutine and wait for
// Run to return before considering shutdown complete.
type TargetGroupBindingController struct {
	client        kubernetes.Interface
	dynamicClient dynamic.Interface
	logger        *slog.Logger
}

// NewTargetGroupBindingController returns a TargetGroupBindingController
// using client to list Services/EndpointSlices/Nodes and dynamicClient to
// watch and update TargetGroupBinding objects. A nil logger defaults to
// slog.Default().
func NewTargetGroupBindingController(client kubernetes.Interface, dynamicClient dynamic.Interface, logger *slog.Logger) *TargetGroupBindingController {
	if logger == nil {
		logger = slog.Default()
	}

	return &TargetGroupBindingController{client: client, dynamicClient: dynamicClient, logger: logger}
}

// Run garbage-collects any leftover mirror Service from an earlier fjord
// version (see garbageCollectLegacyMirrorServices), then starts
// TargetGroupBindingController's informers and targetGroupBindingWorkers
// reconcile goroutines, and blocks until ctx is done. It then waits for
// every in-flight reconcile to finish before returning, so it never leaks a
// goroutine past Run returning.
func (c *TargetGroupBindingController) Run(ctx context.Context) error {
	if err := c.garbageCollectLegacyMirrorServices(ctx); err != nil {
		return fmt.Errorf("garbage collect legacy mirror services: %w", err)
	}

	dynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(c.dynamicClient, 0)
	tgbInformer := dynamicFactory.ForResource(TargetGroupBindingGVR).Informer()

	coreFactory := informers.NewSharedInformerFactory(c.client, 0)
	services := coreFactory.Core().V1().Services()
	endpointSlices := coreFactory.Discovery().V1().EndpointSlices()
	nodes := coreFactory.Core().V1().Nodes()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	if err := c.addEventHandlers(queue, tgbInformer, services.Informer(), endpointSlices.Informer(), nodes.Informer()); err != nil {
		return err
	}

	dynamicFactory.Start(ctx.Done())
	coreFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), tgbInformer.HasSynced, services.Informer().HasSynced, endpointSlices.Informer().HasSynced, nodes.Informer().HasSynced) {
		queue.ShutDown()

		return fmt.Errorf("wait for informer cache sync: %w", ctx.Err())
	}

	var wg sync.WaitGroup

	for range targetGroupBindingWorkers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			c.runWorker(ctx, queue, tgbInformer.GetIndexer(), services.Lister(), endpointSlices.Lister(), nodes.Lister())
		}()
	}

	<-ctx.Done()
	queue.ShutDown()
	wg.Wait()

	return nil
}

// addEventHandlers registers Run's four informer event handlers -- one per
// watched type -- against queue, so Run itself stays focused on lifecycle
// (start, sync, run workers, shut down) rather than wiring detail.
func (c *TargetGroupBindingController) addEventHandlers(queue workqueue.TypedRateLimitingInterface[string], tgbInformer, serviceInformer, endpointSliceInformer, nodeInformer cache.SharedIndexInformer) error {
	if _, err := tgbInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { enqueueKey(queue, obj) },
		UpdateFunc: func(_, obj any) { enqueueKey(queue, obj) },
		DeleteFunc: func(obj any) { enqueueKey(queue, obj) },
	}); err != nil {
		return fmt.Errorf("add target group binding informer event handler: %w", err)
	}

	if _, err := serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueTargetGroupBindingsForService(queue, tgbInformer.GetIndexer(), obj) },
		UpdateFunc: func(_, obj any) { c.enqueueTargetGroupBindingsForService(queue, tgbInformer.GetIndexer(), obj) },
		DeleteFunc: func(obj any) { c.enqueueTargetGroupBindingsForService(queue, tgbInformer.GetIndexer(), obj) },
	}); err != nil {
		return fmt.Errorf("add service informer event handler: %w", err)
	}

	if _, err := endpointSliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueTargetGroupBindingsForEndpointSlice(queue, tgbInformer.GetIndexer(), obj) },
		UpdateFunc: func(_, obj any) { c.enqueueTargetGroupBindingsForEndpointSlice(queue, tgbInformer.GetIndexer(), obj) },
		DeleteFunc: func(obj any) { c.enqueueTargetGroupBindingsForEndpointSlice(queue, tgbInformer.GetIndexer(), obj) },
	}); err != nil {
		return fmt.Errorf("add endpoint slice informer event handler: %w", err)
	}

	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { enqueueAllTargetGroupBindings(queue, tgbInformer.GetIndexer()) },
		UpdateFunc: func(_, _ any) { enqueueAllTargetGroupBindings(queue, tgbInformer.GetIndexer()) },
		DeleteFunc: func(any) { enqueueAllTargetGroupBindings(queue, tgbInformer.GetIndexer()) },
	}); err != nil {
		return fmt.Errorf("add node informer event handler: %w", err)
	}

	return nil
}

// enqueueKey adds obj's namespace/name key to queue. Key-derivation errors
// (which only occur for malformed cache objects) are dropped rather than
// returned, matching the fire-and-forget shape client-go event handlers
// require.
func enqueueKey(queue workqueue.TypedRateLimitingInterface[string], obj any) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}

	queue.Add(key)
}

// enqueueTargetGroupBindingsForService enqueues every TargetGroupBinding in
// tgbIndexer whose spec.serviceRef.name names the Service carried by obj --
// so a Service's own changes (type, ports) re-trigger reconciliation of
// every TargetGroupBinding referencing it, not just the TargetGroupBinding's
// own changes.
func (c *TargetGroupBindingController) enqueueTargetGroupBindingsForService(queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer, obj any) {
	svc := coerceService(obj)
	if svc == nil {
		return
	}

	c.enqueueTargetGroupBindingsForServiceName(queue, tgbIndexer, svc.Namespace, svc.Name)
}

// enqueueTargetGroupBindingsForEndpointSlice enqueues every
// TargetGroupBinding in tgbIndexer whose spec.serviceRef.name matches the
// discoveryv1.LabelServiceName label on the EndpointSlice carried by obj --
// so a pod becoming ready/unready, or any other EndpointSlice change,
// re-triggers reconciliation of every TargetGroupBinding whose ip-typed
// targets it backs.
func (c *TargetGroupBindingController) enqueueTargetGroupBindingsForEndpointSlice(queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer, obj any) {
	slice := coerceEndpointSlice(obj)
	if slice == nil {
		return
	}

	serviceName := slice.Labels[discoveryv1.LabelServiceName]
	if serviceName == "" {
		return
	}

	c.enqueueTargetGroupBindingsForServiceName(queue, tgbIndexer, slice.Namespace, serviceName)
}

// enqueueTargetGroupBindingsForServiceName enqueues every TargetGroupBinding
// in tgbIndexer, in namespace, whose spec.serviceRef.name equals
// serviceName. It backs both enqueueTargetGroupBindingsForService and
// enqueueTargetGroupBindingsForEndpointSlice, which each derive namespace
// and serviceName from a different watched object.
func (c *TargetGroupBindingController) enqueueTargetGroupBindingsForServiceName(queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer, namespace, serviceName string) {
	for _, item := range tgbIndexer.List() {
		u, ok := item.(*unstructured.Unstructured)
		if !ok || u.GetNamespace() != namespace {
			continue
		}

		tgb, err := decodeTargetGroupBinding(u)
		if err != nil {
			c.logger.Warn("decode target group binding", "name", u.GetNamespace()+"/"+u.GetName(), "error", err)

			continue
		}

		if tgb.Spec.ServiceRef.Name != serviceName {
			continue
		}

		if key, err := cache.MetaNamespaceKeyFunc(u); err == nil {
			queue.Add(key)
		}
	}
}

// enqueueAllTargetGroupBindings enqueues every TargetGroupBinding currently
// in tgbIndexer. A Node's addition, removal, or address change affects
// every instance-typed TargetGroupBinding's targets cluster-wide, with no
// per-Node correlation key to filter by -- unlike a Service or
// EndpointSlice change, which only affects the TargetGroupBindings that
// reference it -- so the Node event handlers reconcile everything rather
// than trying to narrow the set. ip-typed TargetGroupBindings pay a
// harmless extra reconcile that resolves to the same targets.
func enqueueAllTargetGroupBindings(queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer) {
	for _, item := range tgbIndexer.List() {
		u, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		if key, err := cache.MetaNamespaceKeyFunc(u); err == nil {
			queue.Add(key)
		}
	}
}

// coerceService extracts a *corev1.Service from obj, unwrapping a
// cache.DeletedFinalStateUnknown tombstone (delivered by DeleteFunc when the
// informer missed the deletion event itself). It returns nil for any other
// shape.
func coerceService(obj any) *corev1.Service {
	if svc, ok := obj.(*corev1.Service); ok {
		return svc
	}

	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil
	}

	svc, _ := tombstone.Obj.(*corev1.Service)

	return svc
}

// coerceEndpointSlice extracts a *discoveryv1.EndpointSlice from obj,
// unwrapping a cache.DeletedFinalStateUnknown tombstone the same way
// coerceService does for Service. It returns nil for any other shape.
func coerceEndpointSlice(obj any) *discoveryv1.EndpointSlice {
	if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
		return slice
	}

	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil
	}

	slice, _ := tombstone.Obj.(*discoveryv1.EndpointSlice)

	return slice
}

// runWorker pulls keys off queue and reconciles them until queue is shut
// down (by Run, once ctx is done).
func (c *TargetGroupBindingController) runWorker(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer, serviceLister corelisters.ServiceLister, endpointSliceLister discoverylisters.EndpointSliceLister, nodeLister corelisters.NodeLister) {
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}

		if err := c.reconcile(ctx, key, tgbIndexer, serviceLister, endpointSliceLister, nodeLister); err != nil {
			c.logger.Warn("reconcile target group binding", "key", key, "error", err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}

		queue.Done(key)
	}
}

// reconcile resolves the targets for the TargetGroupBinding named by key
// and writes them to its status (see resolveTargets and updateStatus). A
// TargetGroupBinding that no longer exists needs no cleanup: fjord no
// longer creates anything else on its behalf (see
// garbageCollectLegacyMirrorServices for the one-time exception).
func (c *TargetGroupBindingController) reconcile(ctx context.Context, key string, tgbIndexer cache.Indexer, serviceLister corelisters.ServiceLister, endpointSliceLister discoverylisters.EndpointSliceLister, nodeLister corelisters.NodeLister) error {
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("split key %q: %w", key, err)
	}

	item, exists, err := tgbIndexer.GetByKey(key)
	if err != nil {
		return fmt.Errorf("get %q from target group binding indexer: %w", key, err)
	}

	if !exists {
		return nil
	}

	u, ok := item.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected target group binding indexer item type %T for %q", item, key)
	}

	tgb, err := decodeTargetGroupBinding(u)
	if err != nil {
		return fmt.Errorf("decode target group binding %q: %w", key, err)
	}

	if tgb.Spec.TargetGroupARN != "" {
		c.logger.Debug("ignoring targetGroupARN; fjord has no ALB to register targets with",
			"targetGroupBinding", key, "targetGroupARN", tgb.Spec.TargetGroupARN)
	}

	service, err := serviceLister.Services(namespace).Get(tgb.Spec.ServiceRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("referenced service %s/%s not found: %w", namespace, tgb.Spec.ServiceRef.Name, err)
		}

		return fmt.Errorf("get service %s/%s: %w", namespace, tgb.Spec.ServiceRef.Name, err)
	}

	endpointSlices, err := endpointSliceLister.EndpointSlices(namespace).List(labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: service.Name}))
	if err != nil {
		return fmt.Errorf("list endpoint slices for service %s/%s: %w", namespace, service.Name, err)
	}

	nodes, err := nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	targets, targetType, err := resolveTargets(tgb, service, endpointSlices, nodes)
	if err != nil {
		c.logger.Warn("cannot resolve targets", "targetGroupBinding", key, "service", service.Name, "error", err)

		return nil
	}

	if err := c.updateStatus(ctx, u, tgb.Generation, targets, targetType); err != nil {
		return fmt.Errorf("update target group binding status %q: %w", key, err)
	}

	return nil
}

// updateStatus writes observedGeneration, targets, and targetType to u's
// status subresource via the dynamic client, skipping the write entirely
// when the desired status already matches what decodeTargetGroupBindingStatus
// reads back from u -- so a reconcile that resolves the same targets as
// last time never bumps u's resourceVersion, which would otherwise
// re-trigger the TargetGroupBinding informer's UpdateFunc and reconcile
// forever.
func (c *TargetGroupBindingController) updateStatus(ctx context.Context, u *unstructured.Unstructured, observedGeneration int64, targets []target, targetType string) error {
	desired := targetGroupBindingStatus{
		ObservedGeneration: observedGeneration,
		Targets:            targets,
		TargetType:         targetType,
	}

	current, err := decodeTargetGroupBindingStatus(u)
	if err != nil {
		return fmt.Errorf("decode current status: %w", err)
	}

	if reflect.DeepEqual(current, desired) {
		return nil
	}

	statusMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&desired)
	if err != nil {
		return fmt.Errorf("convert status to unstructured: %w", err)
	}

	updated := u.DeepCopy()
	updated.Object["status"] = statusMap

	if _, err := c.dynamicClient.Resource(TargetGroupBindingGVR).Namespace(updated.GetNamespace()).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update status %s/%s: %w", updated.GetNamespace(), updated.GetName(), err)
	}

	return nil
}

// decodeTargetGroupBinding decodes u into fjord's typed targetGroupBinding
// view. spec.targetGroupARN is read separately (see
// targetGroupBindingSpec.TargetGroupARN's doc comment), since it opts out of
// the struct's usual json-tag-driven conversion.
func decodeTargetGroupBinding(u *unstructured.Unstructured) (*targetGroupBinding, error) {
	var tgb targetGroupBinding

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &tgb); err != nil {
		return nil, fmt.Errorf("convert unstructured target group binding: %w", err)
	}

	if arn, found, err := unstructured.NestedString(u.Object, "spec", "targetGroupARN"); err == nil && found {
		tgb.Spec.TargetGroupARN = arn
	}

	return &tgb, nil
}

// decodeTargetGroupBindingStatus decodes u's status subresource into
// fjord's typed view, returning the zero value if u has no status yet (a
// TargetGroupBinding that has never been reconciled).
func decodeTargetGroupBindingStatus(u *unstructured.Unstructured) (targetGroupBindingStatus, error) {
	statusMap, found, err := unstructured.NestedMap(u.Object, "status")
	if err != nil {
		return targetGroupBindingStatus{}, fmt.Errorf("read status: %w", err)
	}

	if !found {
		return targetGroupBindingStatus{}, nil
	}

	var status targetGroupBindingStatus

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(statusMap, &status); err != nil {
		return targetGroupBindingStatus{}, fmt.Errorf("convert unstructured status: %w", err)
	}

	return status, nil
}

// garbageCollectLegacyMirrorServices deletes every Service left behind by
// an earlier fjord version's mirror-Service TargetGroupBinding emulation
// (see this package's doc comment): a Service named "<tgb>-fjord-tgb" (see
// legacyMirrorServiceSuffix), labeled managedByLabelKey=managedByLabelValue,
// and owned by a TargetGroupBinding. It runs once, at Run's startup, since a
// cluster only ever accumulates these on upgrade from a version that still
// created them, not continuously. Deleting an already-gone Service is not
// an error; a Service that lacks fjord's managed-by label is left alone
// regardless of its name, so a user's own same-named, unrelated Service is
// never touched.
func (c *TargetGroupBindingController) garbageCollectLegacyMirrorServices(ctx context.Context) error {
	services, err := c.client.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabelKey + "=" + managedByLabelValue,
	})
	if err != nil {
		return fmt.Errorf("list fjord-managed services: %w", err)
	}

	for i := range services.Items {
		svc := &services.Items[i]
		if !isLegacyMirrorService(svc) {
			continue
		}

		if err := c.client.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete legacy mirror service %s/%s: %w", svc.Namespace, svc.Name, err)
		}

		c.logger.Info("garbage collected legacy mirror service", "service", svc.Namespace+"/"+svc.Name)
	}

	return nil
}

// isLegacyMirrorService reports whether svc carries every trait an earlier
// fjord version's mirror-Service emulation gave a TargetGroupBinding mirror
// Service, beyond the managedByLabelKey=managedByLabelValue label callers
// must already have filtered for via a label selector: the
// legacyMirrorServiceSuffix name suffix, and a controller OwnerReference to
// a TargetGroupBinding.
func isLegacyMirrorService(svc *corev1.Service) bool {
	if !strings.HasSuffix(svc.Name, legacyMirrorServiceSuffix) {
		return false
	}

	for _, ref := range svc.OwnerReferences {
		if ref.Kind == TargetGroupBindingKind && ref.APIVersion == TargetGroupBindingGroup+"/"+TargetGroupBindingVersion {
			return true
		}
	}

	return false
}
