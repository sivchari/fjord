package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// DefaultSTSEndpoint is the AWS_ENDPOINT_URL_STS value Injector injects
	// into IRSA pods by default, pointing the AWS SDK's web identity
	// credential provider at fjord-agent's fake STS instead of the real one.
	DefaultSTSEndpoint = "http://fjord-agent.kube-system.svc:8080"

	// stsEndpointEnvName is the environment variable the AWS SDK reads to
	// override the endpoint it calls for AssumeRoleWithWebIdentity.
	stsEndpointEnvName = "AWS_ENDPOINT_URL_STS"

	// roleARNAnnotation is the ServiceAccount annotation
	// amazon-eks-pod-identity-webhook keys off to decide whether a pod gets
	// IRSA env vars and a projected token volume injected. Injector mirrors
	// this same gate so it only injects stsEndpointEnvName into pods
	// pod-identity-webhook also mutates.
	roleARNAnnotation = "eks.amazonaws.com/role-arn"

	// defaultServiceAccountName is the ServiceAccount a pod runs as when its
	// spec leaves serviceAccountName unset.
	defaultServiceAccountName = "default"

	admissionReviewAPIVersion = "admission.k8s.io/v1"
	admissionReviewKind       = "AdmissionReview"
)

// Injector is a Kubernetes mutating admission webhook that injects
// stsEndpointEnvName into every pod amazon-eks-pod-identity-webhook also
// grants IRSA credentials to (i.e. pods running as a ServiceAccount carrying
// roleARNAnnotation), so the AWS SDK inside those pods calls fjord-agent's
// fake STS instead of the real one for AssumeRoleWithWebIdentity.
type Injector struct {
	client      kubernetes.Interface
	stsEndpoint string
}

// NewInjector returns an Injector that injects stsEndpoint into pods whose
// ServiceAccount, resolved via client, carries roleARNAnnotation.
func NewInjector(client kubernetes.Interface, stsEndpoint string) *Injector {
	return &Injector{client: client, stsEndpoint: stsEndpoint}
}

// Handler returns the http.Handler serving Injector's webhook endpoint.
func (i *Injector) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /inject", i.handleInject)

	return mux
}

// handleInject serves a single AdmissionReview mutation request.
func (i *Injector) handleInject(w http.ResponseWriter, r *http.Request) {
	review, err := decodeAdmissionReview(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	inject, err := i.needsInjection(r.Context(), review.Request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	patch, err := buildSTSEndpointPatch(review.Request, inject, i.stsEndpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	writeAdmissionResponse(w, review.Request.UID, patch)
}

// needsInjection reports whether req's pod should receive stsEndpointEnvName,
// mirroring amazon-eks-pod-identity-webhook's own gate: its ServiceAccount
// must carry roleARNAnnotation. A missing ServiceAccount is treated as "no",
// matching pod-identity-webhook's own fail-open behavior.
func (i *Injector) needsInjection(ctx context.Context, req *admissionv1.AdmissionRequest) (bool, error) {
	pod, err := decodePod(req)
	if err != nil {
		return false, err
	}

	saName := serviceAccountName(pod)

	sa, err := i.client.CoreV1().ServiceAccounts(req.Namespace).Get(ctx, saName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("get %s/%s service account: %w", req.Namespace, saName, err)
	}

	return serviceAccountNeedsInjection(sa), nil
}

// serviceAccountNeedsInjection reports whether sa carries roleARNAnnotation,
// the same condition amazon-eks-pod-identity-webhook checks before injecting
// IRSA env vars into a pod running as sa.
func serviceAccountNeedsInjection(sa *corev1.ServiceAccount) bool {
	_, ok := sa.Annotations[roleARNAnnotation]

	return ok
}

// serviceAccountName returns the ServiceAccount pod runs as, defaulting to
// "default" when its spec leaves serviceAccountName unset.
func serviceAccountName(pod *corev1.Pod) string {
	if pod.Spec.ServiceAccountName != "" {
		return pod.Spec.ServiceAccountName
	}

	return defaultServiceAccountName
}

// patchOperation is a single RFC 6902 JSON Patch operation.
type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// buildSTSEndpointPatch computes the RFC 6902 JSON patch that injects
// stsEndpointEnvName=stsEndpoint into every container and initContainer of
// the pod carried by req, skipping any container that already defines the
// env var. It returns a nil patch when inject is false, req carries no pod
// object, or every container already defines the env var.
func buildSTSEndpointPatch(req *admissionv1.AdmissionRequest, inject bool, stsEndpoint string) ([]byte, error) {
	if !inject {
		return nil, nil
	}

	pod, err := decodePod(req)
	if err != nil {
		return nil, err
	}

	ops := containerEnvPatchOps("/spec/containers", pod.Spec.Containers, stsEndpoint)
	ops = append(ops, containerEnvPatchOps("/spec/initContainers", pod.Spec.InitContainers, stsEndpoint)...)

	if len(ops) == 0 {
		return nil, nil
	}

	patch, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}

	return patch, nil
}

// containerEnvPatchOps returns the JSON patch operations that add
// stsEndpointEnvName=stsEndpoint to every container in containers (addressed
// by basePath, "/spec/containers" or "/spec/initContainers") that does not
// already define it.
func containerEnvPatchOps(basePath string, containers []corev1.Container, stsEndpoint string) []patchOperation {
	var ops []patchOperation

	for i := range containers {
		if hasEnvVar(containers[i].Env, stsEndpointEnvName) {
			continue
		}

		envVar := corev1.EnvVar{Name: stsEndpointEnvName, Value: stsEndpoint}

		if len(containers[i].Env) == 0 {
			ops = append(ops, patchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("%s/%d/env", basePath, i),
				Value: []corev1.EnvVar{envVar},
			})

			continue
		}

		ops = append(ops, patchOperation{
			Op:    "add",
			Path:  fmt.Sprintf("%s/%d/env/-", basePath, i),
			Value: envVar,
		})
	}

	return ops
}

// hasEnvVar reports whether env already defines a variable named name.
func hasEnvVar(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}

	return false
}

// decodePod decodes the Pod object carried by req.
func decodePod(req *admissionv1.AdmissionRequest) (*corev1.Pod, error) {
	if req == nil || len(req.Object.Raw) == 0 {
		return nil, errors.New("admission request carries no object")
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return nil, fmt.Errorf("unmarshal pod: %w", err)
	}

	return &pod, nil
}

// decodeAdmissionReview decodes r's body as an AdmissionReview carrying a
// non-nil request.
func decodeAdmissionReview(r *http.Request) (*admissionv1.AdmissionReview, error) {
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
		return nil, fmt.Errorf("decode admission review: %w", err)
	}

	if review.Request == nil {
		return nil, errors.New("admission review carries no request")
	}

	return &review, nil
}

// writeAdmissionResponse writes an AdmissionReview response allowing the
// request, applying patch (a JSONPatch document) if non-empty.
func writeAdmissionResponse(w http.ResponseWriter, uid types.UID, patch []byte) {
	response := &admissionv1.AdmissionResponse{
		UID:     uid,
		Allowed: true,
	}

	if len(patch) > 0 {
		patchType := admissionv1.PatchTypeJSONPatch
		response.Patch = patch
		response.PatchType = &patchType
	}

	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionReviewAPIVersion, Kind: admissionReviewKind},
		Response: response,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(review)
}
