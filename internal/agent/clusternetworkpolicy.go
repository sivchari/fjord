package agent

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ClusterNetworkPolicyGroup, ClusterNetworkPolicyVersion,
// ClusterNetworkPolicyKind, and ClusterNetworkPolicyPlural identify the
// networking.k8s.aws/v1alpha1 ClusterNetworkPolicy CRD fjord emulates; see
// internal/cluster's EnsureClusterNetworkPolicyCRD, which registers it, and
// CNPController, which reconciles it.
const (
	ClusterNetworkPolicyGroup   = "networking.k8s.aws"
	ClusterNetworkPolicyVersion = "v1alpha1"
	ClusterNetworkPolicyKind    = "ClusterNetworkPolicy"
	ClusterNetworkPolicyPlural  = "clusternetworkpolicies"
)

// ManagedByLabelKey and ManagedByLabelValue mark every NetworkPolicy
// CNPController creates, so it can find and clean up the ones it owns
// without touching NetworkPolicies created by anyone else.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "fjord"
)

// minPort and maxPort bound the port domain TranslateClusterNetworkPolicy
// computes complement ranges over, matching a TCP/UDP port number's valid
// range.
const (
	minPort int32 = 1
	maxPort int32 = 65535
)

// translatableProtocols are the only protocols TranslateClusterNetworkPolicy
// computes an allow-everyone complement range for; a Deny rule restricting
// any other protocol (e.g. SCTP) is reported Unsupported and left
// unrestricted, since fjord cannot both restrict it and safely leave every
// other protocol's default access untouched.
var translatableProtocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}

// ClusterNetworkPolicy is fjord's typed view of the subset of a
// networking.k8s.aws/v1alpha1 ClusterNetworkPolicy spec
// TranslateClusterNetworkPolicy understands. Fields the real CRD carries
// beyond this are simply not read; see internal/cluster's
// ClusterNetworkPolicy CRD manifest, which preserves them on the object
// rather than rejecting or pruning them.
type ClusterNetworkPolicy struct {
	Name    string
	Subject CNPSubject
	Ingress []CNPRule
	Egress  []CNPRule
}

// CNPSubject is a ClusterNetworkPolicy's spec.subject: the namespaces (and,
// unsupported, pods) the policy applies to. Resolving Namespaces against
// the cluster's actual namespaces is the caller's responsibility (see
// CNPController); TranslateClusterNetworkPolicy only reads Pods, to report
// it as unsupported.
type CNPSubject struct {
	Namespaces *metav1.LabelSelector
	Pods       *metav1.LabelSelector
}

// CNPAction is a ClusterNetworkPolicy rule's spec.ingress[].action or
// spec.egress[].action.
type CNPAction string

// CNPActionAllow and CNPActionDeny are ClusterNetworkPolicy's two rule
// actions.
const (
	CNPActionAllow CNPAction = "Allow"
	CNPActionDeny  CNPAction = "Deny"
)

// CNPRule is one entry of a ClusterNetworkPolicy's spec.ingress (or,
// unsupported, spec.egress).
type CNPRule struct {
	Name   string
	Action CNPAction
	From   []CNPPeer
	Ports  []CNPPort
}

// CNPPeer is one entry of a CNPRule's from.
type CNPPeer struct {
	Namespaces *metav1.LabelSelector
}

// CNPPort is one entry of a CNPRule's ports (spec...ports[].portNumber).
type CNPPort struct {
	Protocol corev1.Protocol
	Port     int32
}

// Unsupported describes one construct in a ClusterNetworkPolicy that
// TranslateClusterNetworkPolicy could not translate into an equivalent
// standard NetworkPolicy. Callers must surface it as a warning: silently
// dropping it would leave the construct unenforced without anyone knowing.
type Unsupported struct {
	// Rule names the ingress/egress rule the unsupported construct belongs
	// to, or "" when it applies to the ClusterNetworkPolicy as a whole
	// (e.g. a pod-level subject).
	Rule string
	// Reason describes what could not be translated.
	Reason string
}

// NetworkPolicyName returns the deterministic name
// TranslateClusterNetworkPolicy gives the NetworkPolicy it derives from a
// ClusterNetworkPolicy named cnpName.
func NetworkPolicyName(cnpName string) string {
	return "fjord-cnp-" + cnpName
}

// TranslateClusterNetworkPolicy translates cnp's ingress rules, as they
// apply to a single namespace named namespace, into an equivalent standard
// NetworkPolicy. It returns nil when nothing in cnp translates to a
// meaningful rule; callers must not create a NetworkPolicy in that case,
// since one with an empty ingress list would deny all ingress traffic --
// unlike a ClusterNetworkPolicy with no translatable rules, which leaves
// traffic unrestricted.
//
// ClusterNetworkPolicy carries Deny semantics; standard NetworkPolicy is
// allow-list only. A Deny rule "from NotIn(X) on port P" is translated as
// "P is allowed only from X" plus an explicit allow-everyone rule covering
// every other port/protocol (the endPort-range complement of P) --
// otherwise creating any NetworkPolicy for the namespace would newly deny
// every port and protocol the ClusterNetworkPolicy never mentioned.
//
// It does not translate: egress rules, pod-level subjects, Deny rules whose
// from is not zero or one NotIn namespace match expression, two Deny rules
// restricting the same protocol/port, any protocol other than TCP/UDP, and
// combining more than one ClusterNetworkPolicy or tier/priority (each
// ClusterNetworkPolicy is translated independently). Each is reported as an
// Unsupported entry rather than silently enforced or ignored.
func TranslateClusterNetworkPolicy(cnp *ClusterNetworkPolicy, namespace string) (*networkingv1.NetworkPolicy, []Unsupported) {
	var unsupported []Unsupported

	if cnp.Subject.Pods != nil {
		unsupported = append(unsupported, Unsupported{Reason: "pod-level subject is not supported"})
	}

	for _, rule := range cnp.Egress {
		unsupported = append(unsupported, Unsupported{Rule: rule.Name, Reason: "egress rules are not supported"})
	}

	var ingressRules []networkingv1.NetworkPolicyIngressRule

	for _, rule := range cnp.Ingress {
		if rule.Action != CNPActionAllow {
			continue
		}

		ingressRule, ruleUnsupported := translateAllowRule(&rule)
		unsupported = append(unsupported, ruleUnsupported...)
		ingressRules = append(ingressRules, ingressRule)
	}

	restricted, denyUnsupported := restrictedPorts(cnp.Ingress)
	unsupported = append(unsupported, denyUnsupported...)

	ingressRules = append(ingressRules, restrictedAllowRules(restricted)...)
	ingressRules = append(ingressRules, complementRules(restricted)...)

	if len(ingressRules) == 0 {
		return nil, unsupported
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NetworkPolicyName(cnp.Name),
			Namespace: namespace,
			Labels:    map[string]string{ManagedByLabelKey: ManagedByLabelValue},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     ingressRules,
		},
	}

	return np, unsupported
}

// translateAllowRule translates an Allow rule directly into a
// NetworkPolicyIngressRule: CNP and standard NetworkPolicy agree on Allow
// semantics, so no complement is needed.
func translateAllowRule(rule *CNPRule) (networkingv1.NetworkPolicyIngressRule, []Unsupported) {
	peers, unsupported := translatePeers(rule.From, rule.Name)

	return networkingv1.NetworkPolicyIngressRule{
		From:  peers,
		Ports: translatePorts(rule.Ports),
	}, unsupported
}

// translatePeers translates peers' namespace selectors into
// NetworkPolicyPeers, reporting a peer with no namespace selector (e.g. a
// pod-only peer) as unsupported and dropping it. An empty peers (matching
// CNP's "from omitted matches every source") translates to a nil from,
// which standard NetworkPolicy also reads as "every source".
func translatePeers(peers []CNPPeer, ruleName string) ([]networkingv1.NetworkPolicyPeer, []Unsupported) {
	if len(peers) == 0 {
		return nil, nil
	}

	var (
		out         []networkingv1.NetworkPolicyPeer
		unsupported []Unsupported
	)

	for _, peer := range peers {
		if peer.Namespaces == nil {
			unsupported = append(unsupported, Unsupported{Rule: ruleName, Reason: "peer has no namespace selector"})

			continue
		}

		out = append(out, networkingv1.NetworkPolicyPeer{NamespaceSelector: peer.Namespaces.DeepCopy()})
	}

	return out, unsupported
}

// translatePorts translates ports into NetworkPolicyPorts. An empty ports
// (matching CNP's "ports omitted matches every port") translates to a nil
// ports, which standard NetworkPolicy also reads as "every port".
func translatePorts(ports []CNPPort) []networkingv1.NetworkPolicyPort {
	if len(ports) == 0 {
		return nil
	}

	out := make([]networkingv1.NetworkPolicyPort, 0, len(ports))

	for _, p := range ports {
		protocol := p.Protocol
		port := intstr.FromInt32(p.Port)
		out = append(out, networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &port})
	}

	return out
}

// restriction is one (protocol, port) pair a Deny rule in cnp.Ingress
// restricts, and the peer still allowed to reach it (nil when the Deny
// rule allows no exceptions -- deny from everyone).
type restriction struct {
	port    int32
	allowed *networkingv1.NetworkPolicyPeer
}

// restrictedPorts collects every (protocol, port) a Deny rule in rules
// restricts, translating each Deny rule's from into the peer allowed to
// still reach that port (see denyAllowedPeer). Any Deny rule -- or
// individual port within one -- it cannot safely translate is reported as
// Unsupported and excluded from the returned map, which leaves that
// restriction unenforced (fjord's fail-open default) rather than silently
// misapplied. Two Deny rules restricting the same protocol/port are both
// excluded and reported, since there is no way to tell which one should
// win.
func restrictedPorts(rules []CNPRule) (map[corev1.Protocol][]restriction, []Unsupported) {
	type key struct {
		protocol corev1.Protocol
		port     int32
	}

	byPort := make(map[key][]restriction)

	var unsupported []Unsupported

	for _, rule := range rules {
		if rule.Action != CNPActionDeny {
			continue
		}

		allowed, ok, ruleUnsupported := denyAllowedPeer(&rule)
		unsupported = append(unsupported, ruleUnsupported...)

		if !ok {
			continue
		}

		for _, port := range rule.Ports {
			if !isTranslatableProtocol(port.Protocol) {
				unsupported = append(unsupported, Unsupported{
					Rule:   rule.Name,
					Reason: fmt.Sprintf("protocol %s is not supported", port.Protocol),
				})

				continue
			}

			k := key{protocol: port.Protocol, port: port.Port}
			byPort[k] = append(byPort[k], restriction{port: port.Port, allowed: allowed})
		}
	}

	restricted := make(map[corev1.Protocol][]restriction)

	for k, rs := range byPort {
		if len(rs) > 1 {
			unsupported = append(unsupported, Unsupported{
				Reason: fmt.Sprintf("port %s/%d is restricted by more than one ingress rule", k.protocol, k.port),
			})

			continue
		}

		restricted[k.protocol] = append(restricted[k.protocol], rs[0])
	}

	for protocol := range restricted {
		slices.SortFunc(restricted[protocol], func(a, b restriction) int { return int(a.port) - int(b.port) })
	}

	return restricted, unsupported
}

// denyAllowedPeer translates a single Deny rule's from into the peer still
// allowed to reach the ports it restricts. ok is false when the rule's
// shape is not one restrictedPorts can safely translate, in which case the
// caller must not restrict any of its ports.
//
// An empty from (deny from everyone, no exceptions) is supported directly.
// A single peer whose namespace selector is exactly one NotIn match
// expression is translated as its complement: "deny from everyone not in
// X" becomes "allow only from X". Anything else (more than one peer, a
// peer with no namespace selector, or a selector that is not a single
// NotIn expression) is unsupported, since fjord cannot safely express its
// complement as a NetworkPolicy allow-list.
func denyAllowedPeer(rule *CNPRule) (peer *networkingv1.NetworkPolicyPeer, ok bool, unsupported []Unsupported) {
	if len(rule.From) == 0 {
		return nil, true, nil
	}

	if len(rule.From) != 1 {
		return nil, false, []Unsupported{{Rule: rule.Name, Reason: "deny rules with more than one from peer are not supported"}}
	}

	const unsupportedShapeReason = "deny rule's peer must be a single NotIn namespace match expression"

	sel := rule.From[0].Namespaces
	if sel == nil || len(sel.MatchLabels) != 0 || len(sel.MatchExpressions) != 1 {
		return nil, false, []Unsupported{{Rule: rule.Name, Reason: unsupportedShapeReason}}
	}

	expr := sel.MatchExpressions[0]
	if expr.Operator != metav1.LabelSelectorOpNotIn || len(expr.Values) == 0 {
		return nil, false, []Unsupported{{Rule: rule.Name, Reason: unsupportedShapeReason}}
	}

	allowed := &networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: expr.Key, Operator: metav1.LabelSelectorOpIn, Values: expr.Values},
			},
		},
	}

	return allowed, true, nil
}

// isTranslatableProtocol reports whether p is one of translatableProtocols.
func isTranslatableProtocol(p corev1.Protocol) bool {
	return slices.Contains(translatableProtocols, p)
}

// restrictedAllowRules builds one NetworkPolicyIngressRule per restriction
// in restricted that has a non-nil allowed peer -- the "P is allowed only
// from X" half of the Deny translation (see
// TranslateClusterNetworkPolicy's doc comment). Restrictions with a nil
// allowed peer (deny from everyone, no exceptions) contribute no rule here.
func restrictedAllowRules(restricted map[corev1.Protocol][]restriction) []networkingv1.NetworkPolicyIngressRule {
	var rules []networkingv1.NetworkPolicyIngressRule

	for _, protocol := range translatableProtocols {
		for _, r := range restricted[protocol] {
			if r.allowed == nil {
				continue
			}

			port := intstr.FromInt32(r.port)
			ruleProtocol := protocol

			rules = append(rules, networkingv1.NetworkPolicyIngressRule{
				From:  []networkingv1.NetworkPolicyPeer{*r.allowed},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &ruleProtocol, Port: &port}},
			})
		}
	}

	return rules
}

// complementRules builds the "allow everyone else" half of the Deny
// translation: for every translatable protocol, the port ranges restricted
// does not restrict, opened to every namespace. It returns nil when
// restricted has no restriction at all (nothing to complement).
func complementRules(restricted map[corev1.Protocol][]restriction) []networkingv1.NetworkPolicyIngressRule {
	if !hasAnyRestriction(restricted) {
		return nil
	}

	var rules []networkingv1.NetworkPolicyIngressRule

	for _, protocol := range translatableProtocols {
		rules = append(rules, complementRulesForProtocol(protocol, restricted[protocol])...)
	}

	return rules
}

// hasAnyRestriction reports whether restricted restricts at least one port
// on any protocol.
func hasAnyRestriction(restricted map[corev1.Protocol][]restriction) bool {
	for _, rs := range restricted {
		if len(rs) > 0 {
			return true
		}
	}

	return false
}

// complementRulesForProtocol returns one NetworkPolicyIngressRule per gap
// in [minPort, maxPort] left open by restrictions (already sorted by port),
// each allowing every namespace. restrictions being empty yields a single
// rule covering the whole port range.
func complementRulesForProtocol(protocol corev1.Protocol, restrictions []restriction) []networkingv1.NetworkPolicyIngressRule {
	var rules []networkingv1.NetworkPolicyIngressRule

	next := minPort

	for _, r := range restrictions {
		if r.port > next {
			rules = append(rules, openRule(protocol, next, r.port-1))
		}

		next = r.port + 1
	}

	if next <= maxPort {
		rules = append(rules, openRule(protocol, next, maxPort))
	}

	return rules
}

// openRule builds a NetworkPolicyIngressRule allowing every namespace on
// protocol, ports start through end inclusive.
func openRule(protocol corev1.Protocol, start, end int32) networkingv1.NetworkPolicyIngressRule {
	ruleProtocol := protocol
	port := intstr.FromInt32(start)

	networkPort := networkingv1.NetworkPolicyPort{Protocol: &ruleProtocol, Port: &port}

	if end != start {
		endPort := end
		networkPort.EndPort = &endPort
	}

	return networkingv1.NetworkPolicyIngressRule{
		From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
		Ports: []networkingv1.NetworkPolicyPort{networkPort},
	}
}
