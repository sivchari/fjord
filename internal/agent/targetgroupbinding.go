package agent

import (
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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

	// legacyMirrorServiceSuffix is the name suffix an earlier fjord version
	// gave every mirror Service it created for a TargetGroupBinding, before
	// TargetGroupBindingController switched to resolving targets and
	// reporting them in status instead (see this package's doc comment).
	// TargetGroupBindingController.garbageCollectLegacyMirrorServices uses
	// it, alongside managedByLabelKey/managedByLabelValue, to find and
	// remove leftovers from that earlier version.
	legacyMirrorServiceSuffix = "-fjord-tgb"

	// managedByLabelKey and managedByLabelValue marked every mirror Service
	// an earlier fjord version created (see legacyMirrorServiceSuffix) as
	// fjord-managed. TargetGroupBindingController.garbageCollectLegacyMirrorServices
	// still checks for this label, so it never deletes a same-named Service
	// it did not itself create.
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "fjord"

	// targetTypeIP and targetTypeInstance are the two targetType values the
	// upstream elbv2.k8s.aws/v1beta1 CRD defines. targetTypeIP is also the
	// default effectiveTargetType applies when spec.targetType is empty,
	// matching the upstream CRD's own default.
	targetTypeIP       = "ip"
	targetTypeInstance = "instance"
)

// TargetGroupBindingGVR is the dynamic client GroupVersionResource for
// elbv2.k8s.aws/v1beta1 TargetGroupBinding.
var TargetGroupBindingGVR = schema.GroupVersionResource{
	Group:    TargetGroupBindingGroup,
	Version:  TargetGroupBindingVersion,
	Resource: TargetGroupBindingResource,
}

// ErrSelectorlessService is returned by resolveTargets when the
// TargetGroupBinding's referenced Service carries no selector: fjord
// resolves ip-typed targets from that Service's EndpointSlices, which
// Kubernetes' own EndpointSlice controller only ever populates for a
// Service with a selector.
var ErrSelectorlessService = errors.New("target group binding: referenced service has no selector")

// ErrServicePortNotFound is returned by resolveTargets when the
// TargetGroupBinding's spec.serviceRef.port does not match any port on the
// referenced Service.
var ErrServicePortNotFound = errors.New("target group binding: referenced service port not found")

// ErrUnsupportedServiceType is returned by resolveTargets when
// spec.targetType is "instance" but the referenced Service is neither
// type: NodePort nor type: LoadBalancer: instance targets are a node
// address paired with the Service's nodePort, and only those two Service
// types have one allocated.
var ErrUnsupportedServiceType = errors.New("target group binding: instance target type requires a NodePort or LoadBalancer service")

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
// TargetGroupBindingController.reconcile). TargetType selects how
// resolveTargets resolves targets: "ip" (the default, applied by
// effectiveTargetType when this is empty) resolves pod IPs from the
// referenced Service's EndpointSlices; "instance" resolves node addresses
// paired with the Service's nodePort.
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

// target is a single resolved backend TargetGroupBindingController reports
// in a TargetGroupBinding's status.targets: for targetType ip, a pod IP
// paired with the container port real traffic would reach; for targetType
// instance, a node's address paired with the referenced Service's
// nodePort.
type target struct {
	Address string `json:"address"`
	Port    int32  `json:"port"`
}

// targetGroupBindingStatus is fjord's typed view of a TargetGroupBinding's
// status subresource, written by TargetGroupBindingController.reconcile and
// read back by decodeTargetGroupBindingStatus to detect a no-op update.
type targetGroupBindingStatus struct {
	ObservedGeneration int64    `json:"observedGeneration"`
	Targets            []target `json:"targets"`
	TargetType         string   `json:"targetType"`
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

// effectiveTargetType returns tgb.Spec.TargetType, defaulting to
// targetTypeIP when it is empty -- matching the upstream
// elbv2.k8s.aws/v1beta1 CRD's own default.
func effectiveTargetType(tgb *targetGroupBinding) string {
	if tgb.Spec.TargetType == "" {
		return targetTypeIP
	}

	return tgb.Spec.TargetType
}

// resolveTargets resolves tgb's targets from service, matching a real ALB's
// targetType semantics: ip (see resolveIPTargets) registers pod IPs
// directly, instance (see resolveInstanceTargets) registers node addresses
// via the Service's nodePort. It returns the resolved targets sorted by
// address then port (see sortTargets), so repeated reconciles that resolve
// the same target set never churn the TargetGroupBinding's status, along
// with the effective targetType (the "ip" default already applied; see
// effectiveTargetType).
func resolveTargets(tgb *targetGroupBinding, service *corev1.Service, endpointSlices []*discoveryv1.EndpointSlice, nodes []*corev1.Node) ([]target, string, error) {
	targetType := effectiveTargetType(tgb)

	var (
		targets []target
		err     error
	)

	switch targetType {
	case targetTypeInstance:
		targets, err = resolveInstanceTargets(service, tgb.Spec.ServiceRef.Port, nodes)
	default:
		targets, err = resolveIPTargets(service, tgb.Spec.ServiceRef.Port, endpointSlices)
	}

	if err != nil {
		return nil, targetType, err
	}

	sortTargets(targets)

	return targets, targetType, nil
}

// resolveIPTargets resolves targetType ip's targets: every Ready endpoint
// address across endpointSlices (the referenced service's own
// EndpointSlices, selected by the discoveryv1.LabelServiceName label; see
// TargetGroupBindingController.reconcile), paired with the container port
// each EndpointSlice assigns the name portRef resolves to on service (see
// resolveServicePort and endpointSlicePort).
//
// It returns an error wrapping ErrSelectorlessService if service carries no
// selector, or ErrServicePortNotFound if portRef does not match any of
// service's ports.
func resolveIPTargets(service *corev1.Service, portRef intstr.IntOrString, endpointSlices []*discoveryv1.EndpointSlice) ([]target, error) {
	if len(service.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s/%s: %w", service.Namespace, service.Name, ErrSelectorlessService)
	}

	port, err := resolveServicePort(service, portRef)
	if err != nil {
		return nil, err
	}

	targets := make([]target, 0)

	for _, slice := range endpointSlices {
		containerPort, ok := endpointSlicePort(slice, port.Name)
		if !ok {
			continue
		}

		for _, ep := range slice.Endpoints {
			if !endpointReady(&ep) {
				continue
			}

			for _, addr := range ep.Addresses {
				targets = append(targets, target{Address: addr, Port: containerPort})
			}
		}
	}

	return targets, nil
}

// resolveInstanceTargets resolves targetType instance's targets: every
// distinct node address (see nodeAddress, shared with
// LoadBalancerController) paired with the nodePort portRef resolves to on
// service (see resolveServicePort).
//
// It returns an error wrapping ErrUnsupportedServiceType if service is
// neither type: NodePort nor type: LoadBalancer (the only two Service
// types the API server allocates a nodePort for), or ErrServicePortNotFound
// if portRef does not match any of service's ports.
func resolveInstanceTargets(service *corev1.Service, portRef intstr.IntOrString, nodes []*corev1.Node) ([]target, error) {
	if service.Spec.Type != corev1.ServiceTypeNodePort && service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil, fmt.Errorf("service %s/%s: %w", service.Namespace, service.Name, ErrUnsupportedServiceType)
	}

	port, err := resolveServicePort(service, portRef)
	if err != nil {
		return nil, err
	}

	targets := make([]target, 0, len(nodes))

	if port.NodePort == 0 {
		return targets, nil
	}

	seen := make(map[string]struct{}, len(nodes))

	for _, node := range nodes {
		addr := nodeAddress(node)
		if addr == "" {
			continue
		}

		if _, ok := seen[addr]; ok {
			continue
		}

		seen[addr] = struct{}{}

		targets = append(targets, target{Address: addr, Port: port.NodePort})
	}

	return targets, nil
}

// endpointSlicePort returns the port number slice.Ports assigns to the port
// named portName (service's own resolved port name -- "" for a Service
// with a single, unnamed port), and whether one was found. A slice with no
// matching port (e.g. because it backs a different, unrelated Service port)
// contributes no targets.
func endpointSlicePort(slice *discoveryv1.EndpointSlice, portName string) (int32, bool) {
	for _, p := range slice.Ports {
		name := ""
		if p.Name != nil {
			name = *p.Name
		}

		if name != portName {
			continue
		}

		if p.Port == nil {
			return 0, false
		}

		return *p.Port, true
	}

	return 0, false
}

// endpointReady reports whether ep should be treated as ready to receive
// traffic. discoveryv1.EndpointConditions.Ready's zero value is nil, which
// upstream defines as "true" for backward compatibility, not "false", so a
// nil condition counts as ready.
func endpointReady(ep *discoveryv1.Endpoint) bool {
	return ep.Conditions.Ready == nil || *ep.Conditions.Ready
}

// sortTargets sorts targets by address then port in place, so
// resolveTargets returns a deterministic order and repeated reconciles that
// resolve the same target set produce byte-identical status.targets.
func sortTargets(targets []target) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Address != targets[j].Address {
			return targets[i].Address < targets[j].Address
		}

		return targets[i].Port < targets[j].Port
	})
}
