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

	// podIdentityFullURIEnvName is the environment variable the AWS SDK
	// reads to fetch container credentials from a custom endpoint, matching
	// what the official amazon-eks-pod-identity-webhook injects for EKS Pod
	// Identity (see internal/cluster.EnsurePodIdentity's package doc for why
	// fjord's own Injector, rather than that official webhook, injects it
	// here).
	podIdentityFullURIEnvName = "AWS_CONTAINER_CREDENTIALS_FULL_URI"

	// podIdentityAuthFileEnvName and podIdentityAuthFileEnvNameGo name the
	// file holding the token that authenticates the container credentials
	// request. The two AWS SDK families disagree on the variable name:
	// botocore (Python/CLI) reads AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE
	// (the name the upstream webhook injects), while the Go SDK reads
	// AWS_CONTAINER_CREDENTIALS_TOKEN_FILE. Injecting both makes fjord work
	// regardless of the workload's SDK.
	podIdentityAuthFileEnvName   = "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"
	podIdentityAuthFileEnvNameGo = "AWS_CONTAINER_CREDENTIALS_TOKEN_FILE"

	// podIdentityProjectedVolumeName, podIdentityProjectedMountPath, and
	// podIdentityProjectedFileName match the official
	// amazon-eks-pod-identity-webhook's own defaults for the projected
	// ServiceAccount token volume backing EKS Pod Identity, so a pod
	// injected by fjord looks identical to one injected by the real
	// control plane.
	podIdentityProjectedVolumeName = "eks-pod-identity-token"
	podIdentityProjectedMountPath  = "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount"
	podIdentityProjectedFileName   = "eks-pod-identity-token"

	// podIdentityTokenExpirationSeconds is the projected token's requested
	// lifetime, matching fjord's own STS session credential TTL
	// (sessionCredentialsTTL in sts.go).
	podIdentityTokenExpirationSeconds = int64(3600)
)

// Injector is a Kubernetes mutating admission webhook that injects into
// pods the credential sources their ServiceAccount is configured for:
//
//   - stsEndpointEnvName, into every pod amazon-eks-pod-identity-webhook
//     also grants IRSA credentials to (i.e. pods running as a
//     ServiceAccount carrying roleARNAnnotation), so the AWS SDK inside
//     those pods calls fjord-agent's fake STS instead of the real one for
//     AssumeRoleWithWebIdentity.
//   - podIdentityFullURIEnvName and a projected ServiceAccount token
//     volume, into every pod running as a ServiceAccount carrying a
//     registered PodIdentityAssociation. Real EKS Pod Identity injection is
//     driven by amazon-eks-pod-identity-webhook watching a ConfigMap the
//     EKS control plane populates (see internal/cluster.EnsurePodIdentity's
//     doc comment); fjord has no such control plane, so its own Injector
//     performs this injection instead, gated on podIdentityStore.
type Injector struct {
	client           kubernetes.Interface
	stsEndpoint      string
	podIdentityStore PodIdentityStore
}

// NewInjector returns an Injector that injects stsEndpoint into pods whose
// ServiceAccount, resolved via client, carries roleARNAnnotation, and (when
// podIdentityStore is non-nil) injects EKS Pod Identity's credential source
// into pods whose ServiceAccount has a PodIdentityAssociation registered in
// podIdentityStore.
func NewInjector(client kubernetes.Interface, stsEndpoint string, podIdentityStore PodIdentityStore) *Injector {
	return &Injector{client: client, stsEndpoint: stsEndpoint, podIdentityStore: podIdentityStore}
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

	opts, err := i.injectionOptionsFor(r.Context(), review.Request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	patch, err := buildInjectionPatch(review.Request, opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	writeAdmissionResponse(w, review.Request.UID, patch)
}

// injectionOptions selects which credential sources buildInjectionPatch
// injects into a pod: stsEndpoint non-empty injects stsEndpointEnvName, and
// injectPodIdentity injects EKS Pod Identity's env var and projected token
// volume.
type injectionOptions struct {
	stsEndpoint       string
	injectPodIdentity bool
}

// injectionOptionsFor resolves which credential sources req's pod should
// receive, based on its ServiceAccount: the IRSA STS endpoint override if
// the ServiceAccount carries roleARNAnnotation (mirroring
// amazon-eks-pod-identity-webhook's own gate), and EKS Pod Identity's
// credential source if the ServiceAccount has a PodIdentityAssociation
// registered in i.podIdentityStore. A missing ServiceAccount yields neither,
// matching pod-identity-webhook's own fail-open behavior.
func (i *Injector) injectionOptionsFor(ctx context.Context, req *admissionv1.AdmissionRequest) (injectionOptions, error) {
	pod, err := decodePod(req)
	if err != nil {
		return injectionOptions{}, err
	}

	saName := serviceAccountName(pod)

	sa, err := i.client.CoreV1().ServiceAccounts(req.Namespace).Get(ctx, saName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return injectionOptions{}, nil
	}

	if err != nil {
		return injectionOptions{}, fmt.Errorf("get %s/%s service account: %w", req.Namespace, saName, err)
	}

	var opts injectionOptions
	if serviceAccountNeedsInjection(sa) {
		opts.stsEndpoint = i.stsEndpoint
	}

	injectPodIdentity, err := i.needsPodIdentityInjection(ctx, req.Namespace, saName)
	if err != nil {
		return injectionOptions{}, err
	}

	opts.injectPodIdentity = injectPodIdentity

	return opts, nil
}

// needsPodIdentityInjection reports whether the ServiceAccount named
// serviceAccount in namespace has a PodIdentityAssociation registered in
// i.podIdentityStore. It reports false without error when i.podIdentityStore
// is nil (Pod Identity injection disabled) or no association is registered.
func (i *Injector) needsPodIdentityInjection(ctx context.Context, namespace, serviceAccount string) (bool, error) {
	if i.podIdentityStore == nil {
		return false, nil
	}

	_, err := i.podIdentityStore.GetBySA(ctx, namespace, serviceAccount)
	if errors.Is(err, ErrPodIdentityAssociationNotFound) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("look up pod identity association for %s/%s: %w", namespace, serviceAccount, err)
	}

	return true, nil
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

// buildInjectionPatch computes the RFC 6902 JSON patch for the pod carried
// by req, combining the IRSA endpoint override and the EKS Pod Identity
// credential source (env var plus a projected token volume) that opts
// selects into a single patch document. It returns a nil patch when opts
// selects neither, req carries no pod object, or every container already
// carries every env var opts selects.
func buildInjectionPatch(req *admissionv1.AdmissionRequest, opts injectionOptions) ([]byte, error) {
	if opts.stsEndpoint == "" && !opts.injectPodIdentity {
		return nil, nil
	}

	pod, err := decodePod(req)
	if err != nil {
		return nil, err
	}

	envVars := desiredEnvVars(opts)

	ops := containerEnvPatchOps("/spec/containers", pod.Spec.Containers, envVars)
	ops = append(ops, containerEnvPatchOps("/spec/initContainers", pod.Spec.InitContainers, envVars)...)

	if opts.injectPodIdentity {
		ops = append(ops, podIdentityVolumePatchOps(pod)...)
	}

	if len(ops) == 0 {
		return nil, nil
	}

	patch, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("marshal patch: %w", err)
	}

	return patch, nil
}

// desiredEnvVars returns the credential-related env vars opts selects, in a
// stable order (the IRSA STS endpoint override first, matching the order
// amazon-eks-pod-identity-webhook's own IRSA env vars would appear before
// it).
func desiredEnvVars(opts injectionOptions) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if opts.stsEndpoint != "" {
		envVars = append(envVars, corev1.EnvVar{Name: stsEndpointEnvName, Value: opts.stsEndpoint})
	}

	if opts.injectPodIdentity {
		tokenFile := podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName

		envVars = append(envVars,
			corev1.EnvVar{Name: podIdentityFullURIEnvName, Value: DefaultPodIdentityFullURI},
			// Without a token file env the SDK sends no Authorization token,
			// and the agent rejects the request with 400 "Service account
			// token cannot be empty". Inject both SDK families' names.
			corev1.EnvVar{Name: podIdentityAuthFileEnvName, Value: tokenFile},
			corev1.EnvVar{Name: podIdentityAuthFileEnvNameGo, Value: tokenFile},
		)
	}

	return envVars
}

// containerEnvPatchOps returns the JSON patch operations that add every
// variable in envVars a container does not already define to every
// container in containers (addressed by basePath, "/spec/containers" or
// "/spec/initContainers"). Every container's additions are computed and
// applied in a single "add" operation (or a single array-creating "add" when
// the container starts with no env vars at all), so injecting multiple
// variables into the same container never produces two JSON Patch "add"
// operations targeting the same path (which would silently discard the
// first).
func containerEnvPatchOps(basePath string, containers []corev1.Container, envVars []corev1.EnvVar) []patchOperation {
	var ops []patchOperation

	for i := range containers {
		var toAdd []corev1.EnvVar

		for _, envVar := range envVars {
			if !hasEnvVar(containers[i].Env, envVar.Name) {
				toAdd = append(toAdd, envVar)
			}
		}

		if len(toAdd) == 0 {
			continue
		}

		if len(containers[i].Env) == 0 {
			ops = append(ops, patchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("%s/%d/env", basePath, i),
				Value: toAdd,
			})

			continue
		}

		for _, envVar := range toAdd {
			ops = append(ops, patchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("%s/%d/env/-", basePath, i),
				Value: envVar,
			})
		}
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

// podIdentityVolumePatchOps returns the JSON patch operations that add EKS
// Pod Identity's projected ServiceAccount token volume to pod (unless it
// already carries a volume named podIdentityProjectedVolumeName) and mount it
// into every container and initContainer that does not already mount it.
func podIdentityVolumePatchOps(pod *corev1.Pod) []patchOperation {
	var ops []patchOperation

	if !hasVolume(pod.Spec.Volumes, podIdentityProjectedVolumeName) {
		ops = append(ops, addVolumeOp(pod.Spec.Volumes))
	}

	ops = append(ops, containerVolumeMountPatchOps("/spec/containers", pod.Spec.Containers)...)
	ops = append(ops, containerVolumeMountPatchOps("/spec/initContainers", pod.Spec.InitContainers)...)

	return ops
}

// addVolumeOp returns the JSON patch operation that adds EKS Pod Identity's
// projected ServiceAccount token volume to a pod whose existing volumes are
// volumes.
func addVolumeOp(volumes []corev1.Volume) patchOperation {
	expirationSeconds := podIdentityTokenExpirationSeconds

	volume := corev1.Volume{
		Name: podIdentityProjectedVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          podIdentityAudience,
							ExpirationSeconds: &expirationSeconds,
							Path:              podIdentityProjectedFileName,
						},
					},
				},
			},
		},
	}

	if len(volumes) == 0 {
		return patchOperation{Op: "add", Path: "/spec/volumes", Value: []corev1.Volume{volume}}
	}

	return patchOperation{Op: "add", Path: "/spec/volumes/-", Value: volume}
}

// containerVolumeMountPatchOps returns the JSON patch operations that mount
// EKS Pod Identity's projected token volume into every container in
// containers (addressed by basePath) that does not already mount a volume
// named podIdentityProjectedVolumeName.
func containerVolumeMountPatchOps(basePath string, containers []corev1.Container) []patchOperation {
	var ops []patchOperation

	mount := corev1.VolumeMount{
		Name:      podIdentityProjectedVolumeName,
		MountPath: podIdentityProjectedMountPath,
		ReadOnly:  true,
	}

	for i := range containers {
		if hasVolumeMount(containers[i].VolumeMounts, podIdentityProjectedVolumeName) {
			continue
		}

		if len(containers[i].VolumeMounts) == 0 {
			ops = append(ops, patchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("%s/%d/volumeMounts", basePath, i),
				Value: []corev1.VolumeMount{mount},
			})

			continue
		}

		ops = append(ops, patchOperation{
			Op:    "add",
			Path:  fmt.Sprintf("%s/%d/volumeMounts/-", basePath, i),
			Value: mount,
		})
	}

	return ops
}

// hasVolume reports whether volumes already defines a volume named name.
func hasVolume(volumes []corev1.Volume, name string) bool {
	for i := range volumes {
		if volumes[i].Name == name {
			return true
		}
	}

	return false
}

// hasVolumeMount reports whether mounts already defines a mount of a volume
// named name.
func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
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
