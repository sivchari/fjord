package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// LoadBalancerControllerResyncPeriod is how often LoadBalancerController's
// informers force a full resync, catching a node set change (added,
// removed, or an IP change) even without a triggering watch event on the
// node itself.
const LoadBalancerControllerResyncPeriod = 5 * time.Minute

// LoadBalancerController is a k3s-servicelb ("klipper")-style controller
// for fjord's rask substrate: rask runs the cluster directly in the host's
// network namespace, so there is no docker network to carve a LoadBalancer
// address pool from the way metallb does for fjord's kind backend. Instead,
// LoadBalancerController watches Services and, for every class-less
// type: LoadBalancer Service with no status.loadBalancer.ingress yet,
// claims it by publishing the cluster's node address(es) there -- since
// under hostproc the node IP is host-reachable and kube-proxy already DNATs
// LoadBalancer ingress IPs (and NodePorts) to it.
//
// It never touches a Service whose ingress is already populated, by us or
// by anything else (e.g. metallb), so it can coexist with another
// LoadBalancer implementation without the two racing on status.
type LoadBalancerController struct {
	client kubernetes.Interface

	svcInformer  cache.SharedIndexInformer
	nodeInformer cache.SharedIndexInformer

	// queue holds at most one pending reconcile request, coalescing bursts
	// of informer events (e.g. every Service's initial cache sync) into a
	// single pass.
	queue chan struct{}

	// logf receives one line per claimed Service and per reconcile error,
	// so operators can see which Services fjord claimed a load balancer
	// address for.
	logf func(format string, args ...any)
}

// NewLoadBalancerController returns a LoadBalancerController that
// reconciles via client. It does not start watching until Run is called. A
// nil logf discards log lines.
func NewLoadBalancerController(client kubernetes.Interface, logf func(format string, args ...any)) *LoadBalancerController {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	factory := informers.NewSharedInformerFactory(client, LoadBalancerControllerResyncPeriod)
	svcInformer := factory.Core().V1().Services().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()

	c := &LoadBalancerController{
		client:       client,
		svcInformer:  svcInformer,
		nodeInformer: nodeInformer,
		queue:        make(chan struct{}, 1),
		logf:         logf,
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.enqueue() },
		UpdateFunc: func(any, any) { c.enqueue() },
		DeleteFunc: func(any) { c.enqueue() },
	}

	// AddEventHandler only errors once the informer has already started,
	// which cannot happen before Run starts both informers below.
	_, _ = svcInformer.AddEventHandler(handler)
	_, _ = nodeInformer.AddEventHandler(handler)

	return c
}

// enqueue requests a reconcile pass.
func (c *LoadBalancerController) enqueue() {
	select {
	case c.queue <- struct{}{}:
	default:
	}
}

// Run starts LoadBalancerController's informers and reconciles until ctx is
// canceled, returning nil once it is. Callers should run it in its own
// goroutine; it blocks until ctx is done.
func (c *LoadBalancerController) Run(ctx context.Context) error {
	go c.svcInformer.Run(ctx.Done())
	go c.nodeInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), c.svcInformer.HasSynced, c.nodeInformer.HasSynced) {
		return fmt.Errorf("wait for load balancer controller cache sync: %w", ctx.Err())
	}

	c.enqueue()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.queue:
			if err := c.reconcile(ctx); err != nil {
				c.logf("load balancer reconcile: %v", err)
			}
		}
	}
}

// reconcile computes the cluster's node address(es) from every Node
// currently in LoadBalancerController's informer cache, then claims every
// eligible Service (see reconcileService) currently in that cache with
// them.
func (c *LoadBalancerController) reconcile(ctx context.Context) error {
	nodes, err := c.listNodes()
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	desired := desiredIngress(nodes)

	services, err := c.listServices()
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}

	for _, svc := range services {
		if err := c.reconcileService(ctx, svc, desired); err != nil {
			return err
		}
	}

	return nil
}

// listNodes returns every Node currently in LoadBalancerController's
// informer cache.
func (c *LoadBalancerController) listNodes() ([]*corev1.Node, error) {
	items := c.nodeInformer.GetStore().List()
	out := make([]*corev1.Node, 0, len(items))

	for _, item := range items {
		node, ok := item.(*corev1.Node)
		if !ok {
			return nil, fmt.Errorf("unexpected node informer item type %T", item)
		}

		out = append(out, node)
	}

	return out, nil
}

// listServices returns every Service currently in LoadBalancerController's
// informer cache.
func (c *LoadBalancerController) listServices() ([]*corev1.Service, error) {
	items := c.svcInformer.GetStore().List()
	out := make([]*corev1.Service, 0, len(items))

	for _, item := range items {
		svc, ok := item.(*corev1.Service)
		if !ok {
			return nil, fmt.Errorf("unexpected service informer item type %T", item)
		}

		out = append(out, svc)
	}

	return out, nil
}

// reconcileService claims svc's status.loadBalancer.ingress with desired if
// svc is eligible: type: LoadBalancer, no loadBalancerClass (a Service
// declaring one belongs to a different implementation, e.g. a real cloud
// LB's "eks.amazonaws.com/nlb" -- matching upstream Kubernetes semantics
// that implementations must ignore Services with a class they don't own),
// and status.loadBalancer.ingress currently empty. An already-populated
// ingress -- whether it is what we would set ourselves (already claimed, an
// idempotent no-op) or a foreign value (e.g. metallb got there first) -- is
// left untouched either way, so reconcileService never needs to tell the
// two cases apart.
func (c *LoadBalancerController) reconcileService(ctx context.Context, svc *corev1.Service, desired []corev1.LoadBalancerIngress) error {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil
	}

	if svc.Spec.LoadBalancerClass != nil {
		return nil
	}

	if len(svc.Status.LoadBalancer.Ingress) != 0 {
		return nil
	}

	if len(desired) == 0 {
		return nil
	}

	updated := svc.DeepCopy()
	updated.Status.LoadBalancer.Ingress = desired

	if _, err := c.client.CoreV1().Services(svc.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update load balancer status %s/%s: %w", svc.Namespace, svc.Name, err)
	}

	c.logf("claimed load balancer service %s/%s with node address(es) %v", svc.Namespace, svc.Name, desired)

	return nil
}

// desiredIngress returns the LoadBalancerIngress entries
// LoadBalancerController assigns to every Service it claims: one per
// distinct node address (see nodeAddress), sorted for a deterministic,
// idempotent result across reconcile passes.
func desiredIngress(nodes []*corev1.Node) []corev1.LoadBalancerIngress {
	seen := make(map[string]struct{}, len(nodes))
	addrs := make([]string, 0, len(nodes))

	for _, node := range nodes {
		addr := nodeAddress(node)
		if addr == "" {
			continue
		}

		if _, ok := seen[addr]; ok {
			continue
		}

		seen[addr] = struct{}{}

		addrs = append(addrs, addr)
	}

	sort.Strings(addrs)

	ingress := make([]corev1.LoadBalancerIngress, 0, len(addrs))
	for _, addr := range addrs {
		ingress = append(ingress, corev1.LoadBalancerIngress{IP: addr})
	}

	return ingress
}

// nodeAddress returns node's InternalIP, or its ExternalIP if it has no
// InternalIP, or "" if it has neither.
func nodeAddress(node *corev1.Node) string {
	var externalIP string

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}

		if addr.Type == corev1.NodeExternalIP && externalIP == "" {
			externalIP = addr.Address
		}
	}

	return externalIP
}
