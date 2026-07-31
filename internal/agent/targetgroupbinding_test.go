package agent

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// haroPreviewGatewayService and haroPreviewGatewayTGB mirror the real-world
// haro-preview-gateway TargetGroupBinding/Service pair from the task
// description, used as TestBuildMirrorService's baseline "real manifest"
// case.
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

func haroPreviewGatewayTGB() *targetGroupBinding {
	return &targetGroupBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "haro-preview-gateway",
			Namespace: "haro-system",
			UID:       types.UID("11111111-1111-1111-1111-111111111111"),
		},
		Spec: targetGroupBindingSpec{
			ServiceRef:     serviceReference{Name: "haro-preview-gateway-envoy", Port: intstr.FromInt32(80)},
			TargetGroupARN: "arn:aws:elasticloadbalancing:ap-northeast-1:670823610084:targetgroup/layerone-haro-dev-envoy-gw/32df7db4c1f1d3a7",
			TargetType:     "ip",
		},
	}
}

// buildMirrorServiceTestCase is a single TestBuildMirrorService case.
type buildMirrorServiceTestCase struct {
	name    string
	tgb     *targetGroupBinding
	service *corev1.Service
	want    *corev1.Service
	wantErr error
}

// buildMirrorServiceTestCases returns TestBuildMirrorService's full table,
// combining the success and error cases below (each factored into its own
// function to keep every function short).
func buildMirrorServiceTestCases() []buildMirrorServiceTestCase {
	cases := buildMirrorServiceSuccessCases()

	return append(cases, buildMirrorServiceErrorCases()...)
}

// buildMirrorServiceSuccessCases returns TestBuildMirrorService's cases
// that expect a built mirror Service, each factored into its own function
// to keep every function short.
func buildMirrorServiceSuccessCases() []buildMirrorServiceTestCase {
	return []buildMirrorServiceTestCase{
		haroPreviewGatewayTestCase(),
		namedPortTestCase(),
		multiplePortsTestCase(),
	}
}

// haroPreviewGatewayTestCase is TestBuildMirrorService's baseline "real
// manifest" case (numeric port).
func haroPreviewGatewayTestCase() buildMirrorServiceTestCase {
	return buildMirrorServiceTestCase{
		name:    "haro preview gateway (real manifest, numeric port)",
		tgb:     haroPreviewGatewayTGB(),
		service: haroPreviewGatewayService(),
		want: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "haro-preview-gateway-fjord-tgb",
				Namespace: "haro-system",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "fjord"},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "elbv2.k8s.aws/v1beta1",
						Kind:       "TargetGroupBinding",
						Name:       "haro-preview-gateway",
						UID:        types.UID("11111111-1111-1111-1111-111111111111"),
					},
				},
			},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeLoadBalancer,
				Selector: map[string]string{"app": "envoy", "gateway": "haro-preview-gateway"},
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromInt32(10080), Protocol: corev1.ProtocolTCP},
				},
			},
		},
	}
}

// namedPortTestCase verifies serviceRef.port resolves by name, picking the
// referenced Service's matching port out of several.
func namedPortTestCase() buildMirrorServiceTestCase {
	return buildMirrorServiceTestCase{
		name: "named port",
		tgb: &targetGroupBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: targetGroupBindingSpec{
				ServiceRef: serviceReference{Name: "web", Port: intstr.FromString("https")},
			},
		},
		service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{
					{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP},
					{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
				},
			},
		},
		want: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-fjord-tgb",
				Namespace: "default",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "fjord"},
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "elbv2.k8s.aws/v1beta1", Kind: "TargetGroupBinding", Name: "web"},
				},
			},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeLoadBalancer,
				Selector: map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{
					{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
				},
			},
		},
	}
}

// multiplePortsTestCase verifies only the referenced numeric port is
// mirrored out of several the Service defines.
func multiplePortsTestCase() buildMirrorServiceTestCase {
	return buildMirrorServiceTestCase{
		name: "multiple ports picks only the referenced one",
		tgb: &targetGroupBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
			Spec: targetGroupBindingSpec{
				ServiceRef: serviceReference{Name: "multi", Port: intstr.FromInt32(9090)},
			},
		},
		service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "multi"},
				Ports: []corev1.ServicePort{
					{Name: "metrics", Port: 9100, TargetPort: intstr.FromInt32(9100)},
					{Name: "grpc", Port: 9090, TargetPort: intstr.FromInt32(9091), Protocol: corev1.ProtocolTCP},
					{Name: "admin", Port: 9200, TargetPort: intstr.FromInt32(9200)},
				},
			},
		},
		want: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "multi-fjord-tgb",
				Namespace: "default",
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "fjord"},
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "elbv2.k8s.aws/v1beta1", Kind: "TargetGroupBinding", Name: "multi"},
				},
			},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeLoadBalancer,
				Selector: map[string]string{"app": "multi"},
				Ports: []corev1.ServicePort{
					{Name: "grpc", Port: 9090, TargetPort: intstr.FromInt32(9091), Protocol: corev1.ProtocolTCP},
				},
			},
		},
	}
}

// buildMirrorServiceErrorCases returns TestBuildMirrorService's cases that
// expect buildMirrorService to return an error.
func buildMirrorServiceErrorCases() []buildMirrorServiceTestCase {
	return []buildMirrorServiceTestCase{
		{
			name: "port not found",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: targetGroupBindingSpec{
					ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(9999)},
				},
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
			name: "named port not found",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: targetGroupBindingSpec{
					ServiceRef: serviceReference{Name: "web", Port: intstr.FromString("grpc")},
				},
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
			name: "selector-less service",
			tgb: &targetGroupBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: targetGroupBindingSpec{
					ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)},
				},
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
				},
			},
			wantErr: ErrSelectorlessService,
		},
	}
}

func TestBuildMirrorService(t *testing.T) {
	t.Parallel()

	for _, tt := range buildMirrorServiceTestCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := buildMirrorService(tt.tgb, tt.service)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildMirrorService() error = %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("buildMirrorService() unexpected error: %v", err)
			}

			assertServiceEqual(t, got, tt.want)
		})
	}
}

// assertServiceEqual compares the fields buildMirrorService actually sets,
// rather than requiring reflect.DeepEqual over the full corev1.Service
// zero-value surface (which would make every test case list every unset
// field).
func assertServiceEqual(t *testing.T, got, want *corev1.Service) {
	t.Helper()

	if got.Name != want.Name || got.Namespace != want.Namespace {
		t.Errorf("name/namespace = %s/%s, want %s/%s", got.Namespace, got.Name, want.Namespace, want.Name)
	}

	if got.Labels[managedByLabelKey] != want.Labels[managedByLabelKey] {
		t.Errorf("labels[%s] = %q, want %q", managedByLabelKey, got.Labels[managedByLabelKey], want.Labels[managedByLabelKey])
	}

	assertOwnerReference(t, got, want)
	assertServiceSpec(t, got, want)
}

// assertOwnerReference checks got carries exactly one OwnerReference,
// matching want's on the fields buildMirrorService sets, and marked as a
// controller reference with BlockOwnerDeletion.
func assertOwnerReference(t *testing.T, got, want *corev1.Service) {
	t.Helper()

	if len(got.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences = %v, want exactly 1", got.OwnerReferences)
	}

	gotOwner, wantOwner := got.OwnerReferences[0], want.OwnerReferences[0]
	if gotOwner.APIVersion != wantOwner.APIVersion || gotOwner.Kind != wantOwner.Kind || gotOwner.Name != wantOwner.Name || gotOwner.UID != wantOwner.UID {
		t.Errorf("OwnerReferences[0] = %+v, want %+v", gotOwner, wantOwner)
	}

	if gotOwner.Controller == nil || !*gotOwner.Controller {
		t.Error("OwnerReferences[0].Controller = nil/false, want true")
	}

	if gotOwner.BlockOwnerDeletion == nil || !*gotOwner.BlockOwnerDeletion {
		t.Error("OwnerReferences[0].BlockOwnerDeletion = nil/false, want true")
	}
}

// assertServiceSpec checks got.Spec matches want.Spec: type: LoadBalancer,
// the same selector entries, and exactly the one port buildMirrorService
// ever builds.
func assertServiceSpec(t *testing.T, got, want *corev1.Service) {
	t.Helper()

	if got.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Spec.Type = %v, want %v", got.Spec.Type, corev1.ServiceTypeLoadBalancer)
	}

	if len(got.Spec.Selector) != len(want.Spec.Selector) {
		t.Errorf("Spec.Selector = %v, want %v", got.Spec.Selector, want.Spec.Selector)
	}

	for k, v := range want.Spec.Selector {
		if got.Spec.Selector[k] != v {
			t.Errorf("Spec.Selector[%q] = %q, want %q", k, got.Spec.Selector[k], v)
		}
	}

	if len(got.Spec.Ports) != 1 || len(want.Spec.Ports) != 1 {
		t.Fatalf("Spec.Ports = %v, want exactly 1 port matching %v", got.Spec.Ports, want.Spec.Ports)
	}

	if got.Spec.Ports[0] != want.Spec.Ports[0] {
		t.Errorf("Spec.Ports[0] = %+v, want %+v", got.Spec.Ports[0], want.Spec.Ports[0])
	}
}

// TestBuildMirrorServiceCopiesSelector verifies the mirror's selector is an
// independent copy: mutating it afterward must not affect the source
// Service's own selector map.
func TestBuildMirrorServiceCopiesSelector(t *testing.T) {
	t.Parallel()

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{{Port: 80}},
		},
	}
	tgb := &targetGroupBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       targetGroupBindingSpec{ServiceRef: serviceReference{Name: "web", Port: intstr.FromInt32(80)}},
	}

	mirror, err := buildMirrorService(tgb, service)
	if err != nil {
		t.Fatalf("buildMirrorService() error: %v", err)
	}

	mirror.Spec.Selector["app"] = "mutated"

	if service.Spec.Selector["app"] != "web" {
		t.Errorf("source service selector mutated: %v", service.Spec.Selector)
	}
}

func TestMirrorServiceName(t *testing.T) {
	t.Parallel()

	if got, want := mirrorServiceName("haro-preview-gateway"), "haro-preview-gateway-fjord-tgb"; got != want {
		t.Errorf("mirrorServiceName() = %q, want %q", got, want)
	}
}
