package cluster

import (
	"context"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sivchari/fjord/internal/agent"
)

// targetGroupBindingCRDName is the CustomResourceDefinition's own name,
// following Kubernetes' required "<plural>.<group>" convention.
const targetGroupBindingCRDName = agent.TargetGroupBindingResource + "." + agent.TargetGroupBindingGroup

// targetGroupBindingCRDAPIVersion and targetGroupBindingCRDKind are the
// apiVersion/kind of the CustomResourceDefinition object itself (a built-in
// Kubernetes API type, not the elbv2.k8s.aws/v1beta1 group it defines).
const (
	targetGroupBindingCRDAPIVersion = "apiextensions.k8s.io/v1"
	targetGroupBindingCRDKind       = "CustomResourceDefinition"
)

// EnsureTargetGroupBindingCRD installs the elbv2.k8s.aws/v1beta1
// TargetGroupBinding CustomResourceDefinition that fjord's TargetGroupBinding
// emulation (internal/agent.TargetGroupBindingController, run inside
// fjord-agent) reconciles. Real AWS Load Balancer Controller users apply a
// TargetGroupBinding to bind a Service to an ALB/NLB TargetGroup; fjord has
// no ALB, so it emulates the binding by mirroring the referenced Service as
// a type: LoadBalancer Service instead (see the controller's package doc).
//
// This is always installed -- fjord's EKS emulation treats it as a
// built-in feature, not an opt-in one -- but a TargetGroupBinding has no
// effect unless fjord-agent (which runs the reconciling controller) is also
// deployed, i.e. --enable-auth.
//
// It is idempotent: applyObject creates the CRD on a first call and
// JSON-merge-patches it on every later call.
func EnsureTargetGroupBindingCRD(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface) error {
	mapper, err := newRESTMapper(client.Discovery())
	if err != nil {
		return fmt.Errorf("build rest mapper: %w", err)
	}

	obj, err := targetGroupBindingCRDObject()
	if err != nil {
		return fmt.Errorf("build target group binding crd: %w", err)
	}

	if err := applyObject(ctx, dynamicClient, mapper, obj); err != nil {
		return fmt.Errorf("apply target group binding crd: %w", err)
	}

	return nil
}

// targetGroupBindingCRDObject builds the TargetGroupBinding
// CustomResourceDefinition as a typed apiextensionsv1.CustomResourceDefinition
// (for readability and compile-time field checking of its OpenAPI schema),
// converted to unstructured for applyObject's dynamic client.
//
// The schema only names the fields fjord's controller reads (see
// internal/agent's targetGroupBindingSpec); every other field the upstream
// CRD defines is preserved but not validated, via
// x-kubernetes-preserve-unknown-fields, so applying a real
// TargetGroupBinding manifest (e.g. one also meant for a real AWS Load
// Balancer Controller) never fails validation here. serviceRef.port carries
// x-kubernetes-int-or-string, matching upstream: it accepts either a port
// number or a named port.
func targetGroupBindingCRDObject() (*unstructured.Unstructured, error) {
	preserveUnknownFields := true

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: targetGroupBindingCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: agent.TargetGroupBindingGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   agent.TargetGroupBindingResource,
				Singular: "targetgroupbinding",
				Kind:     agent.TargetGroupBindingKind,
				ListKind: agent.TargetGroupBindingKind + "List",
			},
			Scope:    apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{targetGroupBindingCRDVersion(preserveUnknownFields)},
		},
	}

	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(crd)
	if err != nil {
		return nil, fmt.Errorf("convert crd to unstructured: %w", err)
	}

	obj := &unstructured.Unstructured{Object: converted}
	obj.SetAPIVersion(targetGroupBindingCRDAPIVersion)
	obj.SetKind(targetGroupBindingCRDKind)

	return obj, nil
}

// targetGroupBindingCRDVersion builds the sole (v1beta1) version this CRD
// serves and stores, with a status subresource (matching upstream) and the
// OpenAPI schema described in targetGroupBindingCRDObject's doc comment.
func targetGroupBindingCRDVersion(preserveUnknownFields bool) apiextensionsv1.CustomResourceDefinitionVersion {
	return apiextensionsv1.CustomResourceDefinitionVersion{
		Name:    agent.TargetGroupBindingVersion,
		Served:  true,
		Storage: true,
		Subresources: &apiextensionsv1.CustomResourceSubresources{
			Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
		},
		Schema: &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"spec":   targetGroupBindingSpecSchema(preserveUnknownFields),
					"status": {Type: "object", XPreserveUnknownFields: &preserveUnknownFields},
				},
			},
		},
	}
}

// targetGroupBindingSpecSchema builds the OpenAPI schema for
// TargetGroupBinding's spec: the fields fjord's controller reads
// (serviceRef, targetGroupARN, targetType, ipAddressType, vpcID,
// networking), plus x-kubernetes-preserve-unknown-fields so unmodeled
// fields survive validation untouched.
func targetGroupBindingSpecSchema(preserveUnknownFields bool) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type:                   "object",
		Required:               []string{"targetGroupARN", "serviceRef"},
		XPreserveUnknownFields: &preserveUnknownFields,
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"targetGroupARN": {Type: "string"},
			"targetType": {
				Type: "string",
				Enum: []apiextensionsv1.JSON{
					{Raw: []byte(`"instance"`)},
					{Raw: []byte(`"ip"`)},
				},
			},
			"ipAddressType": {
				Type: "string",
				Enum: []apiextensionsv1.JSON{
					{Raw: []byte(`"ipv4"`)},
					{Raw: []byte(`"dualstack"`)},
				},
			},
			"vpcID": {Type: "string"},
			"serviceRef": {
				Type:     "object",
				Required: []string{"name", "port"},
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"name": {Type: "string"},
					"port": {XIntOrString: true},
				},
			},
			"networking": {
				Type:                   "object",
				XPreserveUnknownFields: &preserveUnknownFields,
			},
		},
	}
}
