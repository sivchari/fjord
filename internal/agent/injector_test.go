package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// admissionRequestForPod builds an AdmissionRequest whose Object carries
// pod, matching the shape kube-apiserver sends a mutating webhook for a Pod
// CREATE.
func admissionRequestForPod(t *testing.T, pod *corev1.Pod) *admissionv1.AdmissionRequest {
	t.Helper()

	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}

	return &admissionv1.AdmissionRequest{
		Namespace: "default",
		Object:    runtime.RawExtension{Raw: raw},
	}
}

// stsEndpointPatchTestCase is a single TestBuildSTSEndpointPatch case.
type stsEndpointPatchTestCase struct {
	name        string
	pod         *corev1.Pod
	inject      bool
	stsEndpoint string
	want        []patchOperation
}

// stsEndpointPatchTestCases holds TestBuildSTSEndpointPatch's cases, factored
// out as a package-level value so the test function itself stays short.
var stsEndpointPatchTestCases = []stsEndpointPatchTestCase{
	{
		name:   "inject false yields no patch",
		inject: false,
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		stsEndpoint: DefaultSTSEndpoint,
		want:        nil,
	},
	{
		name:   "single container with no env gets an env array added",
		inject: true,
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		stsEndpoint: DefaultSTSEndpoint,
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/0/env",
				Value: []corev1.EnvVar{
					{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint},
				},
			},
		},
	},
	{
		name:   "container with existing env appends via env/-",
		inject: true,
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Env: []corev1.EnvVar{{Name: "OTHER", Value: "x"}}},
			},
		}},
		stsEndpoint: DefaultSTSEndpoint,
		want: []patchOperation{
			{
				Op:    "add",
				Path:  "/spec/containers/0/env/-",
				Value: corev1.EnvVar{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint},
			},
		},
	},
	{
		name:   "container already carrying the env var is skipped",
		inject: true,
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Env: []corev1.EnvVar{{Name: stsEndpointEnvName, Value: "http://already-set"}}},
			},
		}},
		stsEndpoint: DefaultSTSEndpoint,
		want:        nil,
	},
	{
		name:   "initContainers are patched alongside containers",
		inject: true,
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init"}},
			Containers:     []corev1.Container{{Name: "app"}},
		}},
		stsEndpoint: DefaultSTSEndpoint,
		want: []patchOperation{
			{
				Op:    "add",
				Path:  "/spec/containers/0/env",
				Value: []corev1.EnvVar{{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint}},
			},
			{
				Op:    "add",
				Path:  "/spec/initContainers/0/env",
				Value: []corev1.EnvVar{{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint}},
			},
		},
	},
	{
		name:   "mixed containers only patch the ones missing the env var",
		inject: true,
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "already-set", Env: []corev1.EnvVar{{Name: stsEndpointEnvName, Value: "x"}}},
				{Name: "needs-it"},
			},
		}},
		stsEndpoint: "http://fjord-agent.kube-system.svc:8080",
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/1/env",
				Value: []corev1.EnvVar{
					{Name: stsEndpointEnvName, Value: "http://fjord-agent.kube-system.svc:8080"},
				},
			},
		},
	},
}

func TestBuildSTSEndpointPatch(t *testing.T) {
	t.Parallel()

	for _, tt := range stsEndpointPatchTestCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := admissionRequestForPod(t, tt.pod)

			got, err := buildSTSEndpointPatch(req, tt.inject, tt.stsEndpoint)
			if err != nil {
				t.Fatalf("buildSTSEndpointPatch: %v", err)
			}

			assertPatchEqual(t, got, tt.want)
		})
	}
}

func TestBuildSTSEndpointPatch_NoObject(t *testing.T) {
	t.Parallel()

	req := &admissionv1.AdmissionRequest{Namespace: "default"}

	if _, err := buildSTSEndpointPatch(req, true, DefaultSTSEndpoint); err == nil {
		t.Error("buildSTSEndpointPatch with no object = nil error, want error")
	}
}

// assertPatchEqual decodes got (nil-able JSON patch bytes) and compares it
// against want, ignoring nothing: both must describe the same operations in
// the same order.
func assertPatchEqual(t *testing.T, got []byte, want []patchOperation) {
	t.Helper()

	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("patch = %s, want none", got)
		}

		return
	}

	if len(got) == 0 {
		t.Fatalf("patch = none, want %+v", want)
	}

	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}

	var gotOps, wantOps []patchOperation

	if err := json.Unmarshal(got, &gotOps); err != nil {
		t.Fatalf("unmarshal got patch %s: %v", got, err)
	}

	if err := json.Unmarshal(wantJSON, &wantOps); err != nil {
		t.Fatalf("unmarshal want patch: %v", err)
	}

	gotNormalized, err := json.Marshal(gotOps)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}

	wantNormalized, err := json.Marshal(wantOps)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}

	if !bytes.Equal(gotNormalized, wantNormalized) {
		t.Errorf("patch = %s, want %s", gotNormalized, wantNormalized)
	}
}

func TestServiceAccountNeedsInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sa   *corev1.ServiceAccount
		want bool
	}{
		{
			name: "role-arn annotation present",
			sa: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{roleARNAnnotation: "arn:aws:iam::000000000000:role/my-role"},
				},
			},
			want: true,
		},
		{
			name: "no annotations",
			sa:   &corev1.ServiceAccount{},
			want: false,
		},
		{
			name: "unrelated annotations only",
			sa: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"other/annotation": "value"},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := serviceAccountNeedsInjection(tt.sa); got != tt.want {
				t.Errorf("serviceAccountNeedsInjection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServiceAccountName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "explicit service account",
			pod:  &corev1.Pod{Spec: corev1.PodSpec{ServiceAccountName: "my-sa"}},
			want: "my-sa",
		},
		{
			name: "unset defaults to default",
			pod:  &corev1.Pod{},
			want: "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := serviceAccountName(tt.pod); got != tt.want {
				t.Errorf("serviceAccountName() = %q, want %q", got, tt.want)
			}
		})
	}
}
