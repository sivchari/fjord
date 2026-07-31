package agent

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TargetGroupBindingGroup, TargetGroupBindingVersion, TargetGroupBindingKind,
// and TargetGroupBindingResource identify the AWS Load Balancer Controller's
// elbv2.k8s.aws/v1beta1 TargetGroupBinding CRD that
// TargetGroupBindingController emulates. They are shared with
// internal/cluster.EnsureTargetGroupBindingCRD, which installs the CRD
// these constants describe, so the two stay in sync.
const (
	// TargetGroupBindingGroup is the CRD's API group.
	TargetGroupBindingGroup = "elbv2.k8s.aws"
	// TargetGroupBindingVersion is the CRD's served/stored version.
	TargetGroupBindingVersion = "v1beta1"
	// TargetGroupBindingKind is the CRD's kind.
	TargetGroupBindingKind = "TargetGroupBinding"
	// TargetGroupBindingResource is the CRD's plural resource name.
	TargetGroupBindingResource = "targetgroupbindings"

	// mirrorServiceSuffix names the mirror LoadBalancer Service
	// buildMirrorService creates for a TargetGroupBinding, appended to the
	// TargetGroupBinding's own name.
	mirrorServiceSuffix = "-fjord-tgb"

	// managedByLabelKey and managedByLabelValue mark every mirror Service
	// buildMirrorService creates as fjord-managed, so
	// TargetGroupBindingController never overwrites or deletes a
	// differently-owned Service that happens to occupy the same
	// deterministic name.
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "fjord"
)

// TargetGroupBindingGVR is the dynamic client GroupVersionResource for
// elbv2.k8s.aws/v1beta1 TargetGroupBinding.
var TargetGroupBindingGVR = schema.GroupVersionResource{
	Group:    TargetGroupBindingGroup,
	Version:  TargetGroupBindingVersion,
	Resource: TargetGroupBindingResource,
}

// ErrSelectorlessService is returned by buildMirrorService when the
// TargetGroupBinding's referenced Service carries no selector: fjord has no
// Endpoints/EndpointSlice-based mirroring, so such a Service cannot be
// mirrored.
var ErrSelectorlessService = errors.New("target group binding: referenced service has no selector")

// ErrServicePortNotFound is returned by buildMirrorService when the
// TargetGroupBinding's spec.serviceRef.port does not match any port on the
// referenced Service.
var ErrServicePortNotFound = errors.New("target group binding: referenced service port not found")

// targetGroupBinding is fjord's typed view of an elbv2.k8s.aws/v1beta1
// TargetGroupBinding, decoded from the unstructured object the dynamic
// client and informer deal in (see decodeTargetGroupBinding). Only the
// fields fjord's emulation reads are modeled; every other field the
// upstream CRD defines (networking, ipAddressType, vpcID, ...) is ignored,
// which the CRD (installed by internal/cluster.EnsureTargetGroupBindingCRD)
// tolerates via x-kubernetes-preserve-unknown-fields.
type targetGroupBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              targetGroupBindingSpec `json:"spec"`
}

// targetGroupBindingSpec is the subset of elbv2.k8s.aws/v1beta1
// TargetGroupBindingSpec fjord's emulation reads. TargetGroupARN is a real
// AWS ARN with no local meaning and is always ignored (see
// TargetGroupBindingController.reconcile). TargetType (instance vs. ip)
// makes no difference to fjord's mirror-Service emulation either: both are
// absorbed into the same type: LoadBalancer Service regardless.
type targetGroupBindingSpec struct {
	ServiceRef serviceReference `json:"serviceRef"`
	TargetType string           `json:"targetType,omitempty"`
	// TargetGroupARN is populated by decodeTargetGroupBinding directly from
	// the unstructured object's spec.targetGroupARN, not via this struct's
	// usual json-tag-driven conversion: the upstream CRD's field is
	// literally named "targetGroupARN", which golangci-lint's tagliatelle
	// check rejects for any camelCase struct tag.
	TargetGroupARN string `json:"-"`
}

// serviceReference identifies the Service and port a TargetGroupBinding
// binds to. Port may be a number or a named port, matching the upstream
// CRD's serviceRef.port (int-or-string).
type serviceReference struct {
	Name string             `json:"name"`
	Port intstr.IntOrString `json:"port"`
}

// mirrorServiceName returns the deterministic name buildMirrorService gives
// the mirror Service for the TargetGroupBinding named tgbName.
func mirrorServiceName(tgbName string) string {
	return tgbName + mirrorServiceSuffix
}

// buildMirrorService builds the desired mirror Service for tgb: a
// type: LoadBalancer Service in the same namespace, reusing service's
// selector and the single port tgb.Spec.ServiceRef.Port identifies on
// service.
//
// fjord has no ALB to register targets with, so instead of mutating service
// in place (which another controller, e.g. Envoy Gateway, may already own
// and reconcile), it creates a separate mirror Service carrying the same
// selector. Giving that mirror type: LoadBalancer reproduces a
// TargetGroupBinding's real meaning locally: "this Service is the
// externally reachable backend". The mirror's EXTERNAL-IP is only populated
// once metallb (`fjord create cluster --with-loadbalancer`) is deployed;
// TargetGroupBindingController logs a warning when it creates a mirror.
//
// tgb.Spec.TargetGroupARN is always ignored (see targetGroupBindingSpec's
// doc comment). tgb.Spec.TargetType does not change the mirror built here
// either: whether real traffic would have reached pods directly (ip) or via
// each node's NodePort (instance), the local mirror routes through the
// referenced Service's own selector regardless.
//
// buildMirrorService returns an error wrapping ErrSelectorlessService if
// service carries no selector, or ErrServicePortNotFound if
// tgb.Spec.ServiceRef.Port does not match any of service's ports.
func buildMirrorService(tgb *targetGroupBinding, service *corev1.Service) (*corev1.Service, error) {
	if len(service.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s/%s: %w", service.Namespace, service.Name, ErrSelectorlessService)
	}

	port, err := resolveServicePort(service, tgb.Spec.ServiceRef.Port)
	if err != nil {
		return nil, err
	}

	selector := make(map[string]string, len(service.Spec.Selector))
	for k, v := range service.Spec.Selector {
		selector[k] = v
	}

	isController := true
	blockOwnerDeletion := true

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mirrorServiceName(tgb.Name),
			Namespace: tgb.Namespace,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         TargetGroupBindingGroup + "/" + TargetGroupBindingVersion,
					Kind:               TargetGroupBindingKind,
					Name:               tgb.Name,
					UID:                tgb.UID,
					Controller:         &isController,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: selector,
			Ports:    []corev1.ServicePort{port},
		},
	}, nil
}

// resolveServicePort returns the ServicePort on service that ref
// identifies: by name when ref is a string, by number otherwise, matching
// how elbv2.k8s.aws/v1beta1's own serviceRef.port (int-or-string) is
// resolved against the referenced Service upstream.
func resolveServicePort(service *corev1.Service, ref intstr.IntOrString) (corev1.ServicePort, error) {
	for _, p := range service.Spec.Ports {
		if ref.Type == intstr.String && p.Name == ref.StrVal {
			return p, nil
		}

		if ref.Type == intstr.Int && p.Port == ref.IntVal {
			return p, nil
		}
	}

	return corev1.ServicePort{}, fmt.Errorf("service %s/%s has no port %q: %w", service.Namespace, service.Name, ref.String(), ErrServicePortNotFound)
}
