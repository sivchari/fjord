package agent

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// haroPreviewGatewayService and haroPreviewGatewayTGB mirror a real-world
// haro-preview-gateway TargetGroupBinding/Service pair, used as
// TestResolveTargets' baseline "real manifest" case.
func haroPreviewGatewayService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "haro-preview-gateway-envoy", Namespace: "haro-system"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "envoy", "gateway": "haro-preview-gateway"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(10080), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func haroPreviewGatewayTGB(targetType string) *targetGroupBinding {
	return &targetGroupBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "haro-preview-gateway",
			Namespace:  "haro-system",
			Generation: 3,
		},
		Spec: targetGroupBindingSpec{
			ServiceRef:     serviceReference{Name: "haro-preview-gateway-envoy", Port: intstr.FromInt32(80)},
			TargetGroupARN: "arn:aws:elasticloadbalancing:ap-northeast-1:123456789012:targetgroup/example-gateway/32df7db4c1f1d3a7",
			TargetType:     targetType,
		},
	}
}

// haroPreviewGatewayEndpointSlice is haroPreviewGatewayService's
// EndpointSlice: two Ready pods and one not-Ready pod, exercising
// TestResolveTargets' "only Ready endpoints" assertion.
func haroPreviewGatewayEndpointSlice() *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "haro-preview-gateway-envoy-abcde",
			Namespace: "haro-system",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "haro-preview-gateway-envoy"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{
			{Name: ptr("http"), Port: ptr(int32(10080)), Protocol: ptr(corev1.ProtocolTCP)},
		},
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.244.0.6"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)}},
			{Addresses: []string{"10.244.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)}},
			{Addresses: []string{"10.244.0.9"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(false)}},
		},
	}
}

// resolveTargetsTestCase is a single TestResolveTargets case.
type resolveTargetsTestCase struct {
	name           string
	tgb            *targetGroupBinding
	service        *corev1.Service
	endpointSlices []*discoveryv1.EndpointSlice
	nodes          []*corev1.Node
	wantTargets    []target
	wantTargetType string
	wantErr        error
}

func resolveTargetsTestCases() []resolveTargetsTestCase {
	cases := resolveTargetsIPTestCases()
	cases = append(cases, resolveTargetsInstanceTestCases()...)

	return append(cases, resolveTargetsErrorCases()...)
}

// resolveTargetsIPTestCases returns TestResolveTargets' targetType ip cases
// (including targetType left empty, which defaults to ip).
func resolveTargetsIPTestCases() []resolveTargetsTestCase {
	return []resolveTargetsTestCase{
		{
			name:           "ip target type resolves ready endpoints only",
			tgb:            haroPreviewGatewayTGB(targetTypeIP),
			service:        haroPreviewGatewayService(),
			endpointSlices: []*discoveryv1.EndpointSlice{haroPreviewGatewayEndpointSlice()},
			wantTargets: []target{
				{Address: "10.244.0.5", Port: 10080},
				{Address: "10.244.0.6", Port: 10080},
			},
			wantTargetType: targetTypeIP,
		},
		{
			name:           "target type absent defaults to ip",
			tgb:            haroPreviewGatewayTGB(""),
			service:        haroPreviewGatewayService(),
			endpointSlices: []*discoveryv1.EndpointSlice{haroPreviewGatewayEndpointSlice()},
			wantTargets: []target{
				{Address: "10.244.0.5", Port: 10080},
				{Address: "10.244.0.6", Port: 10080},
			},
			wantTargetType: targetTypeIP,
		},
		differentServicePortTestCase(),
		noMatchingEndpointSliceTestCase(),
	}
}

// differentServicePortTestCase verifies resolveIPTargets only picks up the
// EndpointSlice port matching the referenced Service port's own name,
// ignoring the slice's other ports.
func differentServicePortTestCase() resolveTargetsTestCase {
	return resolveTargetsTestCase{
		name: "ip target type ignores endpoint slices for a different service port",
		tgb: &targetGroupBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromString("https")}},
		},
		service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)},
					{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443)},
				},
			},
		},
		endpointSlices: []*discoveryv1.EndpointSlice{{
			ObjectMeta: metav1.ObjectMeta{Name: "web-abcde", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
			Ports: []discoveryv1.EndpointPort{
				{Name: ptr("http"), Port: ptr(int32(8080))},
				{Name: ptr("https"), Port: ptr(int32(8443))},
			},
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"10.244.1.1"}},
			},
		}},
		wantTargets:    []target{{Address: "10.244.1.1", Port: 8443}},
		wantTargetType: targetTypeIP,
	}
}

// noMatchingEndpointSliceTestCase verifies resolveIPTargets resolves no
// targets, rather than erroring, when the referenced Service has no
// EndpointSlice yet (e.g. no pods have started).
func noMatchingEndpointSliceTestCase() resolveTargetsTestCase {
	return resolveTargetsTestCase{
		name: "ip target type with no matching endpoint slice resolves no targets",
		tgb: &targetGroupBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)}},
		},
		service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "web"},
				Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
			},
		},
		wantTargets:    []target{},
		wantTargetType: targetTypeIP,
	}
}

// resolveTargetsInstanceTestCases returns TestResolveTargets' targetType
// instance cases.
func resolveTargetsInstanceTestCases() []resolveTargetsTestCase {
	nodePortService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 31080}},
		},
	}

	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.20"},
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
			}},
		},
	}

	return []resolveTargetsTestCase{
		{
			name: "instance target type against a nodeport service",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)}, TargetType: targetTypeInstance},
			},
			service: nodePortService,
			nodes:   nodes,
			wantTargets: []target{
				{Address: "192.168.1.10", Port: 31080},
				{Address: "192.168.1.20", Port: 31080},
			},
			wantTargetType: targetTypeInstance,
		},
	}
}

// resolveTargetsErrorCases returns TestResolveTargets' cases that expect
// resolveTargets to return an error.
func resolveTargetsErrorCases() []resolveTargetsTestCase {
	return []resolveTargetsTestCase{
		{
			name: "ip target type: port not found",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(9999)}},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "web"},
					Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
				},
			},
			wantErr: ErrServicePortNotFound,
		},
		{
			name: "ip target type: selector-less service",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)}},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}},
			},
			wantErr: ErrSelectorlessService,
		},
		{
			name: "instance target type against a clusterip service",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)}, TargetType: targetTypeInstance},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Type:     corev1.ServiceTypeClusterIP,
					Selector: map[string]string{"app": "web"},
					Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
				},
			},
			wantErr: ErrUnsupportedServiceType,
		},
	}
}

func TestResolveTargets(t *testing.T) {
	t.Parallel()

	for _, tt := range resolveTargetsTestCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotTargets, gotTargetType, err := resolveTargets(tt.tgb, tt.service, tt.endpointSlices, tt.nodes)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resolveTargets() error = %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveTargets() unexpected error: %v", err)
			}

			if gotTargetType != tt.wantTargetType {
				t.Errorf("resolveTargets() targetType = %q, want %q", gotTargetType, tt.wantTargetType)
			}

			assertTargetsEqual(t, gotTargets, tt.wantTargets)
		})
	}
}

// assertTargetsEqual compares got and want target-by-target, requiring the
// exact same order -- resolveTargets always sorts its result (see
// sortTargets), so a mismatched order is itself a failure worth reporting.
func assertTargetsEqual(t *testing.T, got, want []target) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("resolveTargets() targets = %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resolveTargets() targets[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestResolveTargetsDeterministicOrdering verifies resolveTargets sorts its
// result by address then port regardless of the input EndpointSlice order,
// so repeated reconciles never churn a TargetGroupBinding's status.
func TestResolveTargetsDeterministicOrdering(t *testing.T) {
	t.Parallel()

	tgb := &targetGroupBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)}},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80}},
		},
	}
	slices := []*discoveryv1.EndpointSlice{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
			Ports:      []discoveryv1.EndpointPort{{Port: ptr(int32(80))}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.244.0.9"}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
			Ports:      []discoveryv1.EndpointPort{{Port: ptr(int32(80))}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.244.0.1"}}},
		},
	}

	got, _, err := resolveTargets(tgb, service, slices, nil)
	if err != nil {
		t.Fatalf("resolveTargets() error: %v", err)
	}

	want := []target{{Address: "10.244.0.1", Port: 80}, {Address: "10.244.0.9", Port: 80}}
	assertTargetsEqual(t, got, want)
}
