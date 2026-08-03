package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// notInPeer returns a CNPPeer matching every namespace except one whose
// kubernetes.io/metadata.name is one of values -- the shape
// TranslateClusterNetworkPolicy translates as "only these values are
// allowed" for a Deny rule (see haro's real ClusterNetworkPolicy, haroCNP
// below).
func notInPeer(values ...string) CNPPeer {
	return CNPPeer{
		Namespaces: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: values},
			},
		},
	}
}

// haroCNP is haro's real ClusterNetworkPolicy (see the task's design doc),
// restricting TCP/8080 (the workspace preview port) to only the
// haro-system namespace.
func haroCNP() ClusterNetworkPolicy {
	return ClusterNetworkPolicy{
		Name: "haro-workspace-preview-port-envoy-gateway-only",
		Subject: CNPSubject{
			Namespaces: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "workspace.fjord.example/user-id", Operator: metav1.LabelSelectorOpExists},
				},
			},
		},
		Ingress: []CNPRule{
			{
				Name:   "deny-non-gateway-namespaces-to-preview-port",
				Action: CNPActionDeny,
				From:   []CNPPeer{notInPeer("haro-system")},
				Ports:  []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 8080}},
			},
		},
	}
}

func TestTranslateClusterNetworkPolicy_Haro(t *testing.T) {
	t.Parallel()

	cnp := haroCNP()
	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "haro-workspace-preview-abc123")

	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", unsupported)
	}

	if np == nil {
		t.Fatal("NetworkPolicy = nil, want non-nil")
	}

	if np.Name != "fjord-cnp-haro-workspace-preview-port-envoy-gateway-only" {
		t.Errorf("name = %q, want %q", np.Name, "fjord-cnp-haro-workspace-preview-port-envoy-gateway-only")
	}

	if np.Namespace != "haro-workspace-preview-abc123" {
		t.Errorf("namespace = %q, want %q", np.Namespace, "haro-workspace-preview-abc123")
	}

	assertPolicyTypesIngress(t, np)

	assertHasRule(t, np, wantRule{
		fromNamespaceKey:    "kubernetes.io/metadata.name",
		fromNamespaceValues: []string{"haro-system"},
		ports:               []wantPort{{corev1.ProtocolTCP, 8080, 0}},
	})
	assertHasRule(t, np, wantRule{
		fromEveryone: true,
		ports:        []wantPort{{corev1.ProtocolTCP, 1, 8079}},
	})
	assertHasRule(t, np, wantRule{
		fromEveryone: true,
		ports:        []wantPort{{corev1.ProtocolTCP, 8081, 65535}},
	})
	assertHasRule(t, np, wantRule{
		fromEveryone: true,
		ports:        []wantPort{{corev1.ProtocolUDP, 1, 65535}},
	})

	if got := len(np.Spec.Ingress); got != 4 {
		t.Errorf("len(ingress) = %d, want 4", got)
	}
}

func TestTranslateClusterNetworkPolicy_AllowRule(t *testing.T) {
	t.Parallel()

	cnp := ClusterNetworkPolicy{
		Name: "allow-from-monitoring",
		Ingress: []CNPRule{
			{
				Name:   "allow-monitoring-scrape",
				Action: CNPActionAllow,
				From: []CNPPeer{{Namespaces: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "monitoring"},
				}}},
				Ports: []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 9090}},
			},
		},
	}

	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")

	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", unsupported)
	}

	if np == nil {
		t.Fatal("NetworkPolicy = nil, want non-nil")
	}

	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("len(ingress) = %d, want 1", len(np.Spec.Ingress))
	}

	rule := np.Spec.Ingress[0]
	if len(rule.From) != 1 || rule.From[0].NamespaceSelector == nil {
		t.Fatalf("from = %+v, want a single namespaceSelector peer", rule.From)
	}

	if got := rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "monitoring" {
		t.Errorf("from namespaceSelector matchLabels = %q, want %q", got, "monitoring")
	}

	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != 9090 || *rule.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("ports = %+v, want [{TCP 9090}]", rule.Ports)
	}
}

// TestTranslateClusterNetworkPolicy_MultiplePorts verifies a Deny rule
// restricting two TCP ports produces a three-segment complement (the gap
// between the two denied ports, plus the ranges below and above both).
func TestTranslateClusterNetworkPolicy_MultiplePorts(t *testing.T) {
	t.Parallel()

	cnp := ClusterNetworkPolicy{
		Name: "deny-two-ports",
		Ingress: []CNPRule{
			{
				Name:   "deny-admin-ports",
				Action: CNPActionDeny,
				From:   []CNPPeer{notInPeer("admin-system")},
				Ports: []CNPPort{
					{Protocol: corev1.ProtocolTCP, Port: 8080},
					{Protocol: corev1.ProtocolTCP, Port: 9090},
				},
			},
		},
	}

	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")

	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", unsupported)
	}

	assertHasRule(t, np, wantRule{
		fromNamespaceKey:    "kubernetes.io/metadata.name",
		fromNamespaceValues: []string{"admin-system"},
		ports:               []wantPort{{corev1.ProtocolTCP, 8080, 0}},
	})
	assertHasRule(t, np, wantRule{
		fromNamespaceKey:    "kubernetes.io/metadata.name",
		fromNamespaceValues: []string{"admin-system"},
		ports:               []wantPort{{corev1.ProtocolTCP, 9090, 0}},
	})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 1, 8079}}})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 8081, 9089}}})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 9091, 65535}}})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolUDP, 1, 65535}}})

	if got := len(np.Spec.Ingress); got != 6 {
		t.Errorf("len(ingress) = %d, want 6", got)
	}
}

func TestTranslateClusterNetworkPolicy_PortBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		port      int32
		wantRules []wantRule
	}{
		{
			name: "denied port is the minimum port (no range below it)",
			port: 1,
			wantRules: []wantRule{
				{fromNamespaceKey: "kubernetes.io/metadata.name", fromNamespaceValues: []string{"admin-system"}, ports: []wantPort{{corev1.ProtocolTCP, 1, 0}}},
				{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 2, 65535}}},
				{fromEveryone: true, ports: []wantPort{{corev1.ProtocolUDP, 1, 65535}}},
			},
		},
		{
			name: "denied port is the maximum port (no range above it)",
			port: 65535,
			wantRules: []wantRule{
				{fromNamespaceKey: "kubernetes.io/metadata.name", fromNamespaceValues: []string{"admin-system"}, ports: []wantPort{{corev1.ProtocolTCP, 65535, 0}}},
				{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 1, 65534}}},
				{fromEveryone: true, ports: []wantPort{{corev1.ProtocolUDP, 1, 65535}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cnp := ClusterNetworkPolicy{
				Name: "deny-boundary",
				Ingress: []CNPRule{
					{
						Name:   "deny-boundary-port",
						Action: CNPActionDeny,
						From:   []CNPPeer{notInPeer("admin-system")},
						Ports:  []CNPPort{{Protocol: corev1.ProtocolTCP, Port: tt.port}},
					},
				},
			}

			np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")
			if len(unsupported) != 0 {
				t.Fatalf("unsupported = %v, want none", unsupported)
			}

			if got := len(np.Spec.Ingress); got != len(tt.wantRules) {
				t.Fatalf("len(ingress) = %d, want %d", got, len(tt.wantRules))
			}

			for _, want := range tt.wantRules {
				assertHasRule(t, np, want)
			}
		})
	}
}

// TestTranslateClusterNetworkPolicy_AdjacentPorts verifies denying two
// adjacent ports (no gap between them) omits the empty middle segment
// rather than emitting an invalid (start > end) range.
func TestTranslateClusterNetworkPolicy_AdjacentPorts(t *testing.T) {
	t.Parallel()

	cnp := ClusterNetworkPolicy{
		Name: "deny-adjacent-ports",
		Ingress: []CNPRule{
			{
				Name:   "deny-adjacent",
				Action: CNPActionDeny,
				From:   []CNPPeer{notInPeer("admin-system")},
				Ports: []CNPPort{
					{Protocol: corev1.ProtocolTCP, Port: 10},
					{Protocol: corev1.ProtocolTCP, Port: 11},
				},
			},
		},
	}

	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")
	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", unsupported)
	}

	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 1, 9}}})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 12, 65535}}})

	// 2 allow rules (port 10, port 11) + 2 TCP complement ranges + 1 UDP
	// complement range = 5; no rule for the empty [11,10] gap between them.
	if got := len(np.Spec.Ingress); got != 5 {
		t.Errorf("len(ingress) = %d, want 5", got)
	}
}

func TestTranslateClusterNetworkPolicy_DenyFromEveryoneNoException(t *testing.T) {
	t.Parallel()

	cnp := ClusterNetworkPolicy{
		Name: "deny-all",
		Ingress: []CNPRule{
			{
				Name:   "deny-metrics-port",
				Action: CNPActionDeny,
				Ports:  []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 9999}},
			},
		},
	}

	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")
	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v, want none", unsupported)
	}

	// No allow rule for port 9999 at all (nobody is excepted), just the
	// complement ranges around it.
	if got := len(np.Spec.Ingress); got != 3 {
		t.Fatalf("len(ingress) = %d, want 3 (two TCP ranges + one UDP range)", got)
	}

	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 1, 9998}}})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolTCP, 10000, 65535}}})
	assertHasRule(t, np, wantRule{fromEveryone: true, ports: []wantPort{{corev1.ProtocolUDP, 1, 65535}}})
}

func TestTranslateClusterNetworkPolicy_NoTranslatableRulesReturnsNil(t *testing.T) {
	t.Parallel()

	cnp := ClusterNetworkPolicy{Name: "empty"}

	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")
	if np != nil {
		t.Errorf("NetworkPolicy = %+v, want nil", np)
	}

	if len(unsupported) != 0 {
		t.Errorf("unsupported = %v, want none", unsupported)
	}
}

// unsupportedTestCase is one TestTranslateClusterNetworkPolicy_Unsupported
// scenario: cnp translates to nil (wantNil) and reports a construct whose
// Reason is wantReason.
type unsupportedTestCase struct {
	name       string
	cnp        ClusterNetworkPolicy
	wantNil    bool
	wantReason string
}

// unsupportedTestCases enumerates every construct
// TranslateClusterNetworkPolicy does not translate.
func unsupportedTestCases() []unsupportedTestCase {
	cases := []unsupportedTestCase{
		{
			name: "pod-level subject",
			cnp: ClusterNetworkPolicy{
				Name:    "pod-subject",
				Subject: CNPSubject{Pods: &metav1.LabelSelector{}},
			},
			wantNil:    true,
			wantReason: "pod-level subject is not supported",
		},
		{
			name: "egress rule",
			cnp: ClusterNetworkPolicy{
				Name: "has-egress",
				Egress: []CNPRule{
					{Name: "deny-egress", Action: CNPActionDeny},
				},
			},
			wantNil:    true,
			wantReason: "egress rules are not supported",
		},
	}

	return append(cases, unsupportedDenyRuleShapeTestCases()...)
}

// unsupportedDenyRuleShapeTestCases enumerates Deny rule shapes
// TranslateClusterNetworkPolicy cannot safely translate (see
// denyAllowedPeer and restrictedPorts).
func unsupportedDenyRuleShapeTestCases() []unsupportedTestCase {
	return []unsupportedTestCase{
		{
			name: "deny rule with more than one from peer",
			cnp: ClusterNetworkPolicy{
				Name: "multi-peer",
				Ingress: []CNPRule{
					{
						Name:   "deny-multi-peer",
						Action: CNPActionDeny,
						From:   []CNPPeer{notInPeer("a"), notInPeer("b")},
						Ports:  []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 80}},
					},
				},
			},
			wantNil:    true,
			wantReason: "deny rules with more than one from peer are not supported",
		},
		{
			name: "deny rule peer is not a NotIn expression",
			cnp: ClusterNetworkPolicy{
				Name: "in-peer",
				Ingress: []CNPRule{
					{
						Name:   "deny-in-peer",
						Action: CNPActionDeny,
						From: []CNPPeer{{Namespaces: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpIn, Values: []string{"a"}},
							},
						}}},
						Ports: []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 80}},
					},
				},
			},
			wantNil:    true,
			wantReason: "deny rule's peer must be a single NotIn namespace match expression",
		},
		{
			name: "protocol other than TCP/UDP",
			cnp: ClusterNetworkPolicy{
				Name: "sctp",
				Ingress: []CNPRule{
					{
						Name:   "deny-sctp",
						Action: CNPActionDeny,
						From:   []CNPPeer{notInPeer("a")},
						Ports:  []CNPPort{{Protocol: corev1.ProtocolSCTP, Port: 80}},
					},
				},
			},
			wantNil:    true,
			wantReason: "protocol SCTP is not supported",
		},
	}
}

func TestTranslateClusterNetworkPolicy_Unsupported(t *testing.T) {
	t.Parallel()

	for _, tt := range unsupportedTestCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			np, unsupported := TranslateClusterNetworkPolicy(&tt.cnp, "app-ns")

			if tt.wantNil && np != nil {
				t.Errorf("NetworkPolicy = %+v, want nil", np)
			}

			if !containsReason(unsupported, tt.wantReason) {
				t.Errorf("unsupported = %v, want a reason containing %q", unsupported, tt.wantReason)
			}
		})
	}
}

// TestTranslateClusterNetworkPolicy_ConflictingDenyRules verifies two Deny
// rules restricting the same protocol/port are reported Unsupported and
// neither restriction is enforced (fjord's fail-open default), rather than
// one silently overriding the other.
func TestTranslateClusterNetworkPolicy_ConflictingDenyRules(t *testing.T) {
	t.Parallel()

	cnp := ClusterNetworkPolicy{
		Name: "conflicting",
		Ingress: []CNPRule{
			{
				Name:   "deny-a",
				Action: CNPActionDeny,
				From:   []CNPPeer{notInPeer("a")},
				Ports:  []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 80}},
			},
			{
				Name:   "deny-b",
				Action: CNPActionDeny,
				From:   []CNPPeer{notInPeer("b")},
				Ports:  []CNPPort{{Protocol: corev1.ProtocolTCP, Port: 80}},
			},
		},
	}

	np, unsupported := TranslateClusterNetworkPolicy(&cnp, "app-ns")

	if !containsReason(unsupported, "port TCP/80 is restricted by more than one ingress rule") {
		t.Errorf("unsupported = %v, want a conflicting-port reason", unsupported)
	}

	// Port 80 is excluded entirely (not restricted by either rule), so
	// nothing is left to translate.
	if np != nil {
		t.Errorf("NetworkPolicy = %+v, want nil", np)
	}
}

func containsReason(unsupported []Unsupported, reason string) bool {
	for _, u := range unsupported {
		if u.Reason == reason {
			return true
		}
	}

	return false
}

func assertPolicyTypesIngress(t *testing.T, np *networkingv1.NetworkPolicy) {
	t.Helper()

	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress]", np.Spec.PolicyTypes)
	}
}

type wantPort struct {
	protocol corev1.Protocol
	port     int32
	endPort  int32
}

type wantRule struct {
	fromEveryone        bool
	fromNamespaceKey    string
	fromNamespaceValues []string
	ports               []wantPort
}

// assertHasRule fails the test unless np.Spec.Ingress contains an entry
// matching want exactly (same from peer, same ports).
func assertHasRule(t *testing.T, np *networkingv1.NetworkPolicy, want wantRule) {
	t.Helper()

	for _, rule := range np.Spec.Ingress {
		if ruleMatches(rule, want) {
			return
		}
	}

	t.Errorf("ingress rules = %+v, want one matching %+v", np.Spec.Ingress, want)
}

func ruleMatches(rule networkingv1.NetworkPolicyIngressRule, want wantRule) bool {
	if !fromMatches(rule.From, want) {
		return false
	}

	if len(rule.Ports) != len(want.ports) {
		return false
	}

	for i, port := range rule.Ports {
		if !portMatches(port, want.ports[i]) {
			return false
		}
	}

	return true
}

func fromMatches(from []networkingv1.NetworkPolicyPeer, want wantRule) bool {
	if want.fromEveryone {
		return len(from) == 1 && from[0].NamespaceSelector != nil &&
			len(from[0].NamespaceSelector.MatchLabels) == 0 && len(from[0].NamespaceSelector.MatchExpressions) == 0
	}

	if want.fromNamespaceKey == "" {
		return true
	}

	if len(from) != 1 || from[0].NamespaceSelector == nil {
		return false
	}

	exprs := from[0].NamespaceSelector.MatchExpressions
	if len(exprs) != 1 {
		return false
	}

	expr := exprs[0]

	return expr.Key == want.fromNamespaceKey && expr.Operator == metav1.LabelSelectorOpIn && stringSlicesEqual(expr.Values, want.fromNamespaceValues)
}

func portMatches(port networkingv1.NetworkPolicyPort, want wantPort) bool {
	if port.Protocol == nil || *port.Protocol != want.protocol {
		return false
	}

	if port.Port == nil || port.Port.IntValue() != int(want.port) {
		return false
	}

	wantEndPort := want.endPort
	if wantEndPort == 0 {
		return port.EndPort == nil
	}

	return port.EndPort != nil && *port.EndPort == wantEndPort
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
