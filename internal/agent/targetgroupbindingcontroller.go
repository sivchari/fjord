package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
)

// targetGroupBindingWorkers is the number of reconcile goroutines
// TargetGroupBindingController.Run starts.
const targetGroupBindingWorkers = 2

// TargetGroupBindingController emulates the AWS Load Balancer Controller's
// TargetGroupBinding (elbv2.k8s.aws/v1beta1) CRD locally: fjord has no ALB
// to register pod/node targets with, so for every TargetGroupBinding it
// instead mirrors the Service it references as a type: LoadBalancer
// Service (see buildMirrorService), reproducing a TargetGroupBinding's real
// meaning -- "this Service is the externally reachable backend" -- without
// mutating the referenced Service, which another controller may already
// own and reconcile.
//
// It watches TargetGroupBinding (via a dynamic informer, since fjord has no
// generated client for it) and Service (so serviceRef changes -- port
// renumbering, selector changes -- are picked up too), reconciling affected
// TargetGroupBindings through a rate-limited work queue. Call Run to start
// it; Run blocks until ctx is done, so callers should run it in its own
// goroutine and wait for Run to return before considering shutdown
// complete.
type TargetGroupBindingController struct {
	client        kubernetes.Interface
	dynamicClient dynamic.Interface
	logger        *slog.Logger
}

// NewTargetGroupBindingController returns a TargetGroupBindingController
// using client to manage mirror Services and dynamicClient to watch
// TargetGroupBinding objects. A nil logger defaults to slog.Default().
func NewTargetGroupBindingController(client kubernetes.Interface, dynamicClient dynamic.Interface, logger *slog.Logger) *TargetGroupBindingController {
	if logger == nil {
		logger = slog.Default()
	}

	return &TargetGroupBindingController{client: client, dynamicClient: dynamicClient, logger: logger}
}

// Run starts TargetGroupBindingController's informers and
// targetGroupBindingWorkers reconcile goroutines, and blocks until ctx is
// done. It then waits for every in-flight reconcile to finish before
// returning, so it never leaks a goroutine past Run returning.
func (c *TargetGroupBindingController) Run(ctx context.Context) error {
	dynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(c.dynamicClient, 0)
	tgbInformer := dynamicFactory.ForResource(TargetGroupBindingGVR).Informer()

	coreFactory := informers.NewSharedInformerFactory(c.client, 0)
	services := coreFactory.Core().V1().Services()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	if _, err := tgbInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { enqueueKey(queue, obj) },
		UpdateFunc: func(_, obj any) { enqueueKey(queue, obj) },
		DeleteFunc: func(obj any) { enqueueKey(queue, obj) },
	}); err != nil {
		return fmt.Errorf("add target group binding informer event handler: %w", err)
	}

	if _, err := services.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueTargetGroupBindingsForService(queue, tgbInformer.GetIndexer(), obj) },
		UpdateFunc: func(_, obj any) { c.enqueueTargetGroupBindingsForService(queue, tgbInformer.GetIndexer(), obj) },
		DeleteFunc: func(obj any) { c.enqueueTargetGroupBindingsForService(queue, tgbInformer.GetIndexer(), obj) },
	}); err != nil {
		return fmt.Errorf("add service informer event handler: %w", err)
	}

	dynamicFactory.Start(ctx.Done())
	coreFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), tgbInformer.HasSynced, services.Informer().HasSynced) {
		queue.ShutDown()

		return fmt.Errorf("wait for informer cache sync: %w", ctx.Err())
	}

	var wg sync.WaitGroup

	for range targetGroupBindingWorkers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			c.runWorker(ctx, queue, tgbInformer.GetIndexer(), services.Lister())
		}()
	}

	<-ctx.Done()
	queue.ShutDown()
	wg.Wait()

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
// tgbIndexer, in the same namespace as the Service carried by obj, whose
// spec.serviceRef.name names that Service -- so a Service's own changes
// (selector, ports) re-trigger reconciliation of every TargetGroupBinding
// referencing it, not just the TargetGroupBinding's own changes.
func (c *TargetGroupBindingController) enqueueTargetGroupBindingsForService(queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer, obj any) {
	svc := coerceService(obj)
	if svc == nil {
		return
	}

	for _, item := range tgbIndexer.List() {
		u, ok := item.(*unstructured.Unstructured)
		if !ok || u.GetNamespace() != svc.Namespace {
			continue
		}

		tgb, err := decodeTargetGroupBinding(u)
		if err != nil {
			c.logger.Warn("decode target group binding", "name", u.GetNamespace()+"/"+u.GetName(), "error", err)

			continue
		}

		if tgb.Spec.ServiceRef.Name != svc.Name {
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

// runWorker pulls keys off queue and reconciles them until queue is shut
// down (by Run, once ctx is done).
func (c *TargetGroupBindingController) runWorker(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], tgbIndexer cache.Indexer, serviceLister corelisters.ServiceLister) {
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}

		if err := c.reconcile(ctx, key, tgbIndexer, serviceLister); err != nil {
			c.logger.Warn("reconcile target group binding", "key", key, "error", err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}

		queue.Done(key)
	}
}

// reconcile brings the mirror Service for the TargetGroupBinding named by
// key in line with its current spec and referenced Service, or deletes the
// mirror if the TargetGroupBinding no longer exists.
func (c *TargetGroupBindingController) reconcile(ctx context.Context, key string, tgbIndexer cache.Indexer, serviceLister corelisters.ServiceLister) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("split key %q: %w", key, err)
	}

	item, exists, err := tgbIndexer.GetByKey(key)
	if err != nil {
		return fmt.Errorf("get %q from target group binding indexer: %w", key, err)
	}

	if !exists {
		return c.deleteMirrorService(ctx, namespace, mirrorServiceName(name))
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

	desired, err := buildMirrorService(tgb, service)
	if err != nil {
		c.logger.Warn("cannot build mirror service", "targetGroupBinding", key, "service", service.Name, "error", err)

		return nil
	}

	return c.applyMirrorService(ctx, desired)
}

// applyMirrorService creates desired if no Service by that name exists yet,
// or updates the existing one in place otherwise. It refuses to touch (and
// returns an error for) a same-named Service fjord did not create, so a
// name collision never clobbers an unrelated Service.
func (c *TargetGroupBindingController) applyMirrorService(ctx context.Context, desired *corev1.Service) error {
	existing, err := c.client.CoreV1().Services(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := c.client.CoreV1().Services(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create mirror service %s/%s: %w", desired.Namespace, desired.Name, err)
		}

		c.logger.Warn("created mirror service; its EXTERNAL-IP stays <pending> unless metallb is deployed",
			"service", desired.Namespace+"/"+desired.Name, "hint", "fjord create cluster --with-loadbalancer")

		return nil
	}

	if err != nil {
		return fmt.Errorf("get mirror service %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if existing.Labels[managedByLabelKey] != managedByLabelValue {
		return fmt.Errorf("service %s/%s already exists and is not managed by fjord", desired.Namespace, desired.Name)
	}

	return c.updateMirrorService(ctx, desired)
}

// updateMirrorService updates the mirror Service named desired.Name to
// desired's selector, ports, labels, and owner references, retrying on
// conflicting concurrent writes. It preserves the API server's
// auto-assigned NodePort so reconciling does not needlessly reallocate one
// on every pass.
func (c *TargetGroupBindingController) updateMirrorService(ctx context.Context, desired *corev1.Service) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := c.client.CoreV1().Services(desired.Namespace).Get(ctx, desired.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get mirror service %s/%s: %w", desired.Namespace, desired.Name, err)
		}

		preserveNodePort(current.Spec.Ports, desired.Spec.Ports)

		current.Labels = desired.Labels
		current.OwnerReferences = desired.OwnerReferences
		current.Spec.Selector = desired.Spec.Selector
		current.Spec.Ports = desired.Spec.Ports

		if _, err := c.client.CoreV1().Services(desired.Namespace).Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update mirror service %s/%s: %w", desired.Namespace, desired.Name, err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("update mirror service %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return nil
}

// preserveNodePort copies current's NodePort onto desired's matching port
// (same port number) in place, when both hold exactly the one port
// buildMirrorService ever builds. Leaving NodePort unset on update would
// have the API server allocate a new one on every reconcile.
func preserveNodePort(current, desired []corev1.ServicePort) {
	if len(current) != 1 || len(desired) != 1 {
		return
	}

	if current[0].Port == desired[0].Port {
		desired[0].NodePort = current[0].NodePort
	}
}

// deleteMirrorService deletes the Service named name in namespace if it
// exists and is managed by fjord (see managedByLabelKey), matching
// applyMirrorService's collision guard. A missing Service is not an error.
func (c *TargetGroupBindingController) deleteMirrorService(ctx context.Context, namespace, name string) error {
	existing, err := c.client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("get mirror service %s/%s: %w", namespace, name, err)
	}

	if existing.Labels[managedByLabelKey] != managedByLabelValue {
		return nil
	}

	if err := c.client.CoreV1().Services(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete mirror service %s/%s: %w", namespace, name, err)
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
