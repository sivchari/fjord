package agent

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// CNPControllerResyncPeriod is how often CNPController's informers force a
// full resync, catching drift (e.g. a managed NetworkPolicy edited by hand)
// even without a triggering watch event.
const CNPControllerResyncPeriod = 5 * time.Minute

// clusterNetworkPolicyGVR is the GroupVersionResource CNPController watches
// and reconciles via a dynamic informer, matching internal/cluster's
// ClusterNetworkPolicy CRD.
var clusterNetworkPolicyGVR = schema.GroupVersionResource{
	Group:    ClusterNetworkPolicyGroup,
	Version:  ClusterNetworkPolicyVersion,
	Resource: ClusterNetworkPolicyPlural,
}

// CNPController watches networking.k8s.aws ClusterNetworkPolicy custom
// resources and Namespaces, and reconciles the standard NetworkPolicy
// objects TranslateClusterNetworkPolicy derives from them into the
// cluster -- fjord's enforcement mechanism, since its CNI (kindnet)
// understands standard NetworkPolicy but not ClusterNetworkPolicy.
type CNPController struct {
	client kubernetes.Interface

	nsInformer  cache.SharedIndexInformer
	cnpInformer cache.SharedIndexInformer

	// queue holds at most one pending reconcile request, coalescing bursts
	// of informer events (e.g. every Namespace's initial cache sync) into a
	// single pass.
	queue chan struct{}

	// logf receives one line per reconcile error or Unsupported construct
	// TranslateClusterNetworkPolicy reports, so operators are warned about
	// constructs fjord does not enforce rather than it happening silently.
	logf func(format string, args ...any)
}

// NewCNPController returns a CNPController that reconciles via client and
// watches ClusterNetworkPolicy custom resources via dynamicClient. It does
// not start watching until Run is called. A nil logf discards warnings.
func NewCNPController(client kubernetes.Interface, dynamicClient dynamic.Interface, logf func(format string, args ...any)) *CNPController {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	factory := informers.NewSharedInformerFactory(client, CNPControllerResyncPeriod)
	nsInformer := factory.Core().V1().Namespaces().Informer()

	dynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, CNPControllerResyncPeriod)
	cnpInformer := dynamicFactory.ForResource(clusterNetworkPolicyGVR).Informer()

	c := &CNPController{
		client:      client,
		nsInformer:  nsInformer,
		cnpInformer: cnpInformer,
		queue:       make(chan struct{}, 1),
		logf:        logf,
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.enqueue() },
		UpdateFunc: func(any, any) { c.enqueue() },
		DeleteFunc: func(any) { c.enqueue() },
	}

	// AddEventHandler only errors once the informer has already started,
	// which cannot happen before Run starts both informers below.
	_, _ = nsInformer.AddEventHandler(handler)
	_, _ = cnpInformer.AddEventHandler(handler)

	return c
}

// enqueue requests a reconcile pass.
func (c *CNPController) enqueue() {
	select {
	case c.queue <- struct{}{}:
	default:
	}
}

// Run starts CNPController's informers and reconciles until ctx is
// canceled, returning nil once it is. Callers should run it in its own
// goroutine; it blocks until ctx is done.
func (c *CNPController) Run(ctx context.Context) error {
	go c.nsInformer.Run(ctx.Done())
	go c.cnpInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), c.nsInformer.HasSynced, c.cnpInformer.HasSynced) {
		return fmt.Errorf("wait for cluster network policy controller cache sync: %w", ctx.Err())
	}

	c.enqueue()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.queue:
			if err := c.reconcile(ctx); err != nil {
				c.logf("cluster network policy reconcile: %v", err)
			}
		}
	}
}

// reconcile recomputes the desired NetworkPolicy set from every
// ClusterNetworkPolicy and Namespace currently in CNPController's informer
// caches, then creates, updates, or deletes managed NetworkPolicies to
// match it.
func (c *CNPController) reconcile(ctx context.Context) error {
	cnps, err := c.listClusterNetworkPolicies()
	if err != nil {
		return fmt.Errorf("list cluster network policies: %w", err)
	}

	namespaces, err := c.listNamespaces()
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}

	desired, err := desiredNetworkPolicies(cnps, namespaces, c.logf)
	if err != nil {
		return err
	}

	existing, err := c.client.NetworkingV1().NetworkPolicies(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: ManagedByLabelKey + "=" + ManagedByLabelValue,
	})
	if err != nil {
		return fmt.Errorf("list managed network policies: %w", err)
	}

	return c.apply(ctx, desired, existing.Items)
}

// decodedCNP pairs a ClusterNetworkPolicy with the custom resource's UID,
// needed to set the NetworkPolicy owner reference desiredNetworkPolicies
// builds.
type decodedCNP struct {
	cnp ClusterNetworkPolicy
	uid types.UID
}

// listClusterNetworkPolicies decodes every ClusterNetworkPolicy currently
// in CNPController's informer cache.
func (c *CNPController) listClusterNetworkPolicies() ([]decodedCNP, error) {
	items := c.cnpInformer.GetStore().List()
	out := make([]decodedCNP, 0, len(items))

	for _, item := range items {
		u, ok := item.(*unstructured.Unstructured)
		if !ok {
			return nil, fmt.Errorf("unexpected cluster network policy informer item type %T", item)
		}

		cnp, err := decodeClusterNetworkPolicy(u)
		if err != nil {
			return nil, err
		}

		out = append(out, decodedCNP{cnp: cnp, uid: u.GetUID()})
	}

	return out, nil
}

// listNamespaces returns every Namespace currently in CNPController's
// informer cache.
func (c *CNPController) listNamespaces() ([]*corev1.Namespace, error) {
	items := c.nsInformer.GetStore().List()
	out := make([]*corev1.Namespace, 0, len(items))

	for _, item := range items {
		ns, ok := item.(*corev1.Namespace)
		if !ok {
			return nil, fmt.Errorf("unexpected namespace informer item type %T", item)
		}

		out = append(out, ns)
	}

	return out, nil
}

// cnpManifest mirrors a networking.k8s.aws/v1alpha1 ClusterNetworkPolicy's
// spec on the wire, used only to decode an unstructured object retrieved
// via the dynamic informer into ClusterNetworkPolicy.
type cnpManifest struct {
	Spec struct {
		Subject struct {
			Namespaces *metav1.LabelSelector `json:"namespaces,omitempty"`
			Pods       *metav1.LabelSelector `json:"pods,omitempty"`
		} `json:"subject"`
		Ingress []cnpRuleManifest `json:"ingress,omitempty"`
		Egress  []cnpRuleManifest `json:"egress,omitempty"`
	} `json:"spec"`
}

// cnpRuleManifest mirrors one entry of a cnpManifest's spec.ingress or
// spec.egress.
type cnpRuleManifest struct {
	Name   string            `json:"name"`
	Action CNPAction         `json:"action"`
	From   []cnpPeerManifest `json:"from,omitempty"`
	Ports  []cnpPortManifest `json:"ports,omitempty"`
}

// cnpPeerManifest mirrors one entry of a cnpRuleManifest's from.
type cnpPeerManifest struct {
	Namespaces *metav1.LabelSelector `json:"namespaces,omitempty"`
}

// cnpPortManifest mirrors one entry of a cnpRuleManifest's ports.
type cnpPortManifest struct {
	PortNumber cnpPortNumberManifest `json:"portNumber"`
}

// cnpPortNumberManifest mirrors a cnpPortManifest's portNumber.
type cnpPortNumberManifest struct {
	Protocol corev1.Protocol `json:"protocol"`
	Port     int32           `json:"port"`
}

// decodeClusterNetworkPolicy decodes obj (a
// networking.k8s.aws/v1alpha1 ClusterNetworkPolicy retrieved via the
// dynamic client) into a ClusterNetworkPolicy.
func decodeClusterNetworkPolicy(obj *unstructured.Unstructured) (ClusterNetworkPolicy, error) {
	var manifest cnpManifest

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &manifest); err != nil {
		return ClusterNetworkPolicy{}, fmt.Errorf("decode cluster network policy %q: %w", obj.GetName(), err)
	}

	return ClusterNetworkPolicy{
		Name: obj.GetName(),
		Subject: CNPSubject{
			Namespaces: manifest.Spec.Subject.Namespaces,
			Pods:       manifest.Spec.Subject.Pods,
		},
		Ingress: decodeRuleManifests(manifest.Spec.Ingress),
		Egress:  decodeRuleManifests(manifest.Spec.Egress),
	}, nil
}

// decodeRuleManifests converts rules (as decoded off the wire) into
// CNPRules.
func decodeRuleManifests(rules []cnpRuleManifest) []CNPRule {
	out := make([]CNPRule, 0, len(rules))

	for _, r := range rules {
		peers := make([]CNPPeer, 0, len(r.From))
		for _, p := range r.From {
			peers = append(peers, CNPPeer(p))
		}

		ports := make([]CNPPort, 0, len(r.Ports))
		for _, p := range r.Ports {
			ports = append(ports, CNPPort{Protocol: p.PortNumber.Protocol, Port: p.PortNumber.Port})
		}

		out = append(out, CNPRule{Name: r.Name, Action: r.Action, From: peers, Ports: ports})
	}

	return out
}

// namespaceMatches reports whether ns's labels match selector. A nil
// selector matches no namespace (spec.subject.namespaces omitted means the
// ClusterNetworkPolicy's subject does not resolve to any namespace fjord
// can enumerate); an empty (non-nil) selector matches every namespace.
func namespaceMatches(selector *metav1.LabelSelector, ns *corev1.Namespace) (bool, error) {
	if selector == nil {
		return false, nil
	}

	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, fmt.Errorf("parse namespace selector: %w", err)
	}

	return sel.Matches(labels.Set(ns.Labels)), nil
}

// desiredKey identifies one desired NetworkPolicy by namespace and name.
type desiredKey struct {
	namespace string
	name      string
}

// desiredNetworkPolicies computes, for every (cnp, namespace) pair whose
// subject matches, the NetworkPolicy TranslateClusterNetworkPolicy derives,
// keyed by namespace/name. It reports every Unsupported construct
// TranslateClusterNetworkPolicy returns via logf.
func desiredNetworkPolicies(cnps []decodedCNP, namespaces []*corev1.Namespace, logf func(format string, args ...any)) (map[desiredKey]*networkingv1.NetworkPolicy, error) {
	desired := make(map[desiredKey]*networkingv1.NetworkPolicy)

	for _, dc := range cnps {
		for _, ns := range namespaces {
			matches, err := namespaceMatches(dc.cnp.Subject.Namespaces, ns)
			if err != nil {
				return nil, fmt.Errorf("cluster network policy %q: %w", dc.cnp.Name, err)
			}

			if !matches {
				continue
			}

			np, unsupported := TranslateClusterNetworkPolicy(&dc.cnp, ns.Name)
			for _, u := range unsupported {
				logf("cluster network policy %q: unsupported (rule=%q): %s", dc.cnp.Name, u.Rule, u.Reason)
			}

			if np == nil {
				continue
			}

			np.OwnerReferences = []metav1.OwnerReference{cnpOwnerReference(&dc)}
			desired[desiredKey{namespace: ns.Name, name: np.Name}] = np
		}
	}

	return desired, nil
}

// cnpOwnerReference builds the OwnerReference a NetworkPolicy
// desiredNetworkPolicies derives from dc carries, so deleting the
// ClusterNetworkPolicy garbage-collects it (a cluster-scoped owner of a
// namespaced dependent is supported by the Kubernetes garbage collector).
func cnpOwnerReference(dc *decodedCNP) metav1.OwnerReference {
	blockOwnerDeletion := true

	return metav1.OwnerReference{
		APIVersion:         ClusterNetworkPolicyGroup + "/" + ClusterNetworkPolicyVersion,
		Kind:               ClusterNetworkPolicyKind,
		Name:               dc.cnp.Name,
		UID:                dc.uid,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

// apply reconciles existing (every NetworkPolicy currently carrying
// ManagedByLabelKey) toward desired: creating missing entries, updating
// changed ones, and deleting orphans (managed NetworkPolicies with no
// matching desired entry -- e.g. their ClusterNetworkPolicy was deleted, or
// its subject no longer matches the namespace).
func (c *CNPController) apply(ctx context.Context, desired map[desiredKey]*networkingv1.NetworkPolicy, existing []networkingv1.NetworkPolicy) error {
	existingByKey := make(map[desiredKey]*networkingv1.NetworkPolicy, len(existing))

	for i := range existing {
		np := &existing[i]
		existingByKey[desiredKey{namespace: np.Namespace, name: np.Name}] = np
	}

	for key, np := range desired {
		current, ok := existingByKey[key]
		if !ok {
			if err := c.create(ctx, np); err != nil {
				return err
			}

			continue
		}

		if err := c.update(ctx, current, np); err != nil {
			return err
		}
	}

	for key, np := range existingByKey {
		if _, ok := desired[key]; ok {
			continue
		}

		if err := c.delete(ctx, np); err != nil {
			return err
		}
	}

	return nil
}

// create creates np, tolerating a concurrent create by another reconcile
// pass.
func (c *CNPController) create(ctx context.Context, np *networkingv1.NetworkPolicy) error {
	_, err := c.client.NetworkingV1().NetworkPolicies(np.Namespace).Create(ctx, np, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("create network policy %s/%s: %w", np.Namespace, np.Name, err)
	}

	return nil
}

// update updates current to desired's spec/owner references/labels if they
// differ, leaving current untouched otherwise.
func (c *CNPController) update(ctx context.Context, current, desired *networkingv1.NetworkPolicy) error {
	if equality.Semantic.DeepEqual(current.Spec, desired.Spec) &&
		equality.Semantic.DeepEqual(current.OwnerReferences, desired.OwnerReferences) &&
		equality.Semantic.DeepEqual(current.Labels, desired.Labels) {
		return nil
	}

	updated := current.DeepCopy()
	updated.Spec = desired.Spec
	updated.OwnerReferences = desired.OwnerReferences
	updated.Labels = desired.Labels

	if _, err := c.client.NetworkingV1().NetworkPolicies(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update network policy %s/%s: %w", updated.Namespace, updated.Name, err)
	}

	return nil
}

// delete deletes np, tolerating a concurrent delete by another reconcile
// pass.
func (c *CNPController) delete(ctx context.Context, np *networkingv1.NetworkPolicy) error {
	err := c.client.NetworkingV1().NetworkPolicies(np.Namespace).Delete(ctx, np.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("delete network policy %s/%s: %w", np.Namespace, np.Name, err)
	}

	return nil
}
