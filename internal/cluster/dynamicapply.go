package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// fieldManager is the field manager name fjord uses when server-side
// applying third-party manifests it does not otherwise model with typed
// client-go objects (see applyManifest and applyObject). Scoping updates to
// this field manager means repeated applies only ever overwrite the fields
// fjord itself sets, leaving fields other actors set at runtime -- notably
// metallb's own controller, which patches its webhook Secret and
// ValidatingWebhookConfiguration caBundle after the initial apply -- alone.
const fieldManager = "fjord"

// newRESTMapper returns a RESTMapper reflecting discoveryClient's current
// API resources. It is a snapshot: callers that apply a manifest defining
// new CustomResourceDefinitions must build a fresh RESTMapper afterwards to
// resolve the CRDs' own kinds, since this one only reflects discovery at the
// time it was built.
func newRESTMapper(discoveryClient discovery.DiscoveryInterface) (meta.RESTMapper, error) {
	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return nil, fmt.Errorf("get api group resources: %w", err)
	}

	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

// applyManifest server-side applies every document in manifest (a
// multi-document YAML stream) via dynamicClient, in document order,
// resolving each document's REST mapping through mapper. It is idempotent;
// see applyObject.
func applyManifest(ctx context.Context, dynamicClient dynamic.Interface, mapper meta.RESTMapper, manifest []byte) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)

	for {
		var obj unstructured.Unstructured

		if err := decoder.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("decode manifest document: %w", err)
		}

		if len(obj.Object) == 0 {
			continue
		}

		if err := applyObject(ctx, dynamicClient, mapper, &obj); err != nil {
			return err
		}
	}
}

// applyObject applies obj via dynamicClient, resolving its GVR and scope
// (namespaced vs. cluster) through mapper. It is idempotent: a first call
// creates obj; every subsequent call JSON-merge-patches it (RFC 7386), which
// only ever sets the fields obj itself declares, leaving fields other actors
// set at runtime (e.g. metallb's controller writing its webhook Secret's
// data, which the manifest declares no `data` for at all, after fjord's
// initial apply) untouched.
func applyObject(ctx context.Context, dynamicClient dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("resolve REST mapping for %s %q: %w", gvk.Kind, obj.GetName(), err)
	}

	var resource dynamic.ResourceInterface = dynamicClient.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = dynamicClient.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	}

	_, err = resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err == nil {
		return patchObject(ctx, resource, obj)
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s %q: %w", gvk.Kind, obj.GetName(), err)
	}

	if _, err := resource.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("create %s %q: %w", gvk.Kind, obj.GetName(), err)
	}

	return nil
}

// patchObject JSON-merge-patches obj (which already exists) via resource; see
// applyObject's doc comment for why a merge patch, rather than a full
// Update, is the right idempotent operation here.
func patchObject(ctx context.Context, resource dynamic.ResourceInterface, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()

	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal %s %q: %w", gvk.Kind, obj.GetName(), err)
	}

	if _, err := resource.Patch(ctx, obj.GetName(), types.MergePatchType, data, metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("apply %s %q: %w", gvk.Kind, obj.GetName(), err)
	}

	return nil
}
