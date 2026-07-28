package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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

// injectionPatchTestCase is a single TestBuildInjectionPatch case.
type injectionPatchTestCase struct {
	name string
	pod  *corev1.Pod
	opts injectionOptions
	want []patchOperation
}

// injectionPatchTestCases holds TestBuildInjectionPatch's cases, factored
// out as a package-level value so the test function itself stays short.
var injectionPatchTestCases = []injectionPatchTestCase{
	{
		name: "no injection selected yields no patch",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{},
		want: nil,
	},
	{
		name: "single container with no env gets an env array added",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint},
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
		name: "container with existing env appends via env/-",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Env: []corev1.EnvVar{{Name: "OTHER", Value: "x"}}},
			},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint},
		want: []patchOperation{
			{
				Op:    "add",
				Path:  "/spec/containers/0/env/-",
				Value: corev1.EnvVar{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint},
			},
		},
	},
	{
		name: "container already carrying the env var is skipped",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Env: []corev1.EnvVar{{Name: stsEndpointEnvName, Value: "http://already-set"}}},
			},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint},
		want: nil,
	},
	{
		name: "initContainers are patched alongside containers",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init"}},
			Containers:     []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint},
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
		name: "mixed containers only patch the ones missing the env var",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "already-set", Env: []corev1.EnvVar{{Name: stsEndpointEnvName, Value: "x"}}},
				{Name: "needs-it"},
			},
		}},
		opts: injectionOptions{stsEndpoint: "http://fjord-agent.kube-system.svc:8080"},
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
	{
		name: "pod identity alone adds env var and projected volume",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{injectPodIdentity: true},
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/0/env",
				Value: []corev1.EnvVar{
					{Name: podIdentityFullURIEnvName, Value: DefaultPodIdentityFullURI},
					{Name: podIdentityAuthFileEnvName, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
					{Name: podIdentityAuthFileEnvNameGo, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
				},
			},
			{
				Op:    "add",
				Path:  "/spec/volumes",
				Value: []corev1.Volume{podIdentityTokenVolume()},
			},
			{
				Op:   "add",
				Path: "/spec/containers/0/volumeMounts",
				Value: []corev1.VolumeMount{
					{Name: podIdentityProjectedVolumeName, MountPath: podIdentityProjectedMountPath, ReadOnly: true},
				},
			},
		},
	},
	{
		name: "sts and pod identity combine into a single env array without colliding",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint, injectPodIdentity: true},
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/0/env",
				Value: []corev1.EnvVar{
					{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint},
					{Name: podIdentityFullURIEnvName, Value: DefaultPodIdentityFullURI},
					{Name: podIdentityAuthFileEnvName, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
					{Name: podIdentityAuthFileEnvNameGo, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
				},
			},
			{
				Op:    "add",
				Path:  "/spec/volumes",
				Value: []corev1.Volume{podIdentityTokenVolume()},
			},
			{
				Op:   "add",
				Path: "/spec/containers/0/volumeMounts",
				Value: []corev1.VolumeMount{
					{Name: podIdentityProjectedVolumeName, MountPath: podIdentityProjectedMountPath, ReadOnly: true},
				},
			},
		},
	},
	{
		name: "pod identity skips a pod that already carries the token volume and mount",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: podIdentityProjectedVolumeName}},
			Containers: []corev1.Container{
				{Name: "app", VolumeMounts: []corev1.VolumeMount{{Name: podIdentityProjectedVolumeName}}},
			},
		}},
		opts: injectionOptions{injectPodIdentity: true},
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/0/env",
				Value: []corev1.EnvVar{
					{Name: podIdentityFullURIEnvName, Value: DefaultPodIdentityFullURI},
					{Name: podIdentityAuthFileEnvName, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
					{Name: podIdentityAuthFileEnvNameGo, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
				},
			},
		},
	},
	{
		name: "sts and awsEndpointURL combine into a single env array in order",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint, awsEndpointURL: "http://kumo.kube-system.svc:4566"},
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/0/env",
				Value: []corev1.EnvVar{
					{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint},
					{Name: awsEndpointURLEnvName, Value: "http://kumo.kube-system.svc:4566"},
				},
			},
		},
	},
	{
		name: "pod identity and awsEndpointURL combine without colliding",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		}},
		opts: injectionOptions{injectPodIdentity: true, awsEndpointURL: "http://kumo.kube-system.svc:4566"},
		want: []patchOperation{
			{
				Op:   "add",
				Path: "/spec/containers/0/env",
				Value: []corev1.EnvVar{
					{Name: awsEndpointURLEnvName, Value: "http://kumo.kube-system.svc:4566"},
					{Name: podIdentityFullURIEnvName, Value: DefaultPodIdentityFullURI},
					{Name: podIdentityAuthFileEnvName, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
					{Name: podIdentityAuthFileEnvNameGo, Value: podIdentityProjectedMountPath + "/" + podIdentityProjectedFileName},
				},
			},
			{
				Op:    "add",
				Path:  "/spec/volumes",
				Value: []corev1.Volume{podIdentityTokenVolume()},
			},
			{
				Op:   "add",
				Path: "/spec/containers/0/volumeMounts",
				Value: []corev1.VolumeMount{
					{Name: podIdentityProjectedVolumeName, MountPath: podIdentityProjectedMountPath, ReadOnly: true},
				},
			},
		},
	},
	{
		name: "container already carrying awsEndpointURLEnvName is skipped for that var only",
		pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Env: []corev1.EnvVar{{Name: awsEndpointURLEnvName, Value: "http://already-set"}}},
			},
		}},
		opts: injectionOptions{stsEndpoint: DefaultSTSEndpoint, awsEndpointURL: "http://kumo.kube-system.svc:4566"},
		want: []patchOperation{
			{
				Op:    "add",
				Path:  "/spec/containers/0/env/-",
				Value: corev1.EnvVar{Name: stsEndpointEnvName, Value: DefaultSTSEndpoint},
			},
		},
	},
}

// podIdentityTokenVolume returns the projected ServiceAccount token volume
// buildInjectionPatch injects for EKS Pod Identity, used to build expected
// test output.
func podIdentityTokenVolume() corev1.Volume {
	expirationSeconds := podIdentityTokenExpirationSeconds

	return corev1.Volume{
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
}

func TestBuildInjectionPatch(t *testing.T) {
	t.Parallel()

	for _, tt := range injectionPatchTestCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := admissionRequestForPod(t, tt.pod)

			got, err := buildInjectionPatch(req, tt.opts)
			if err != nil {
				t.Fatalf("buildInjectionPatch: %v", err)
			}

			assertPatchEqual(t, got, tt.want)
		})
	}
}

func TestBuildInjectionPatch_NoObject(t *testing.T) {
	t.Parallel()

	req := &admissionv1.AdmissionRequest{Namespace: "default"}

	if _, err := buildInjectionPatch(req, injectionOptions{stsEndpoint: DefaultSTSEndpoint}); err == nil {
		t.Error("buildInjectionPatch with no object = nil error, want error")
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

// TestInjectionOptionsFor_PodIdentityAssociation verifies that
// injectionOptionsFor selects EKS Pod Identity injection only when the
// pod's ServiceAccount has a PodIdentityAssociation registered, independent
// of whether it also carries the IRSA role-arn annotation.
// injectionOptionsForTestCase is a single
// TestInjectionOptionsFor_PodIdentityAssociation case.
type injectionOptionsForTestCase struct {
	name               string
	sa                 *corev1.ServiceAccount
	association        *PodIdentityAssociation
	awsEndpointURL     string
	wantSTSEndpoint    string
	wantPodIdentity    bool
	wantAWSEndpointURL string
}

// injectionOptionsForTestCases holds
// TestInjectionOptionsFor_PodIdentityAssociation's cases, factored out as a
// package-level value so the test function itself stays short.
var injectionOptionsForTestCases = []injectionOptionsForTestCase{
	{
		name:            "sa with registered association",
		sa:              &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app-sa", Namespace: "default"}},
		association:     &PodIdentityAssociation{Namespace: "default", ServiceAccount: "app-sa", RoleARN: "arn:aws:iam::000000000000:role/app-role"},
		wantPodIdentity: true,
	},
	{
		name:            "sa with no association",
		sa:              &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app-sa", Namespace: "default"}},
		wantPodIdentity: false,
	},
	{
		name: "sa with both irsa annotation and pod identity association",
		sa: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:        "app-sa",
			Namespace:   "default",
			Annotations: map[string]string{roleARNAnnotation: "arn:aws:iam::000000000000:role/irsa-role"},
		}},
		association:     &PodIdentityAssociation{Namespace: "default", ServiceAccount: "app-sa", RoleARN: "arn:aws:iam::000000000000:role/app-role"},
		wantSTSEndpoint: DefaultSTSEndpoint,
		wantPodIdentity: true,
	},
	{
		name:               "pod identity association with awsEndpointURL configured",
		sa:                 &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app-sa", Namespace: "default"}},
		association:        &PodIdentityAssociation{Namespace: "default", ServiceAccount: "app-sa", RoleARN: "arn:aws:iam::000000000000:role/app-role"},
		awsEndpointURL:     "http://kumo.kube-system.svc:4566",
		wantPodIdentity:    true,
		wantAWSEndpointURL: "http://kumo.kube-system.svc:4566",
	},
	{
		name: "irsa annotation with awsEndpointURL configured",
		sa: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:        "app-sa",
			Namespace:   "default",
			Annotations: map[string]string{roleARNAnnotation: "arn:aws:iam::000000000000:role/irsa-role"},
		}},
		awsEndpointURL:     "http://kumo.kube-system.svc:4566",
		wantSTSEndpoint:    DefaultSTSEndpoint,
		wantAWSEndpointURL: "http://kumo.kube-system.svc:4566",
	},
	{
		name:           "no IAM identity leaves awsEndpointURL uninjected even when configured",
		sa:             &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "app-sa", Namespace: "default"}},
		awsEndpointURL: "http://kumo.kube-system.svc:4566",
	},
}

func TestInjectionOptionsFor_PodIdentityAssociation(t *testing.T) {
	t.Parallel()

	for _, tt := range injectionOptionsForTestCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := fake.NewClientset(tt.sa)
			store := NewInMemoryPodIdentityStore()

			if tt.association != nil {
				if err := store.Put(t.Context(), *tt.association); err != nil {
					t.Fatalf("Put association: %v", err)
				}
			}

			injector := NewInjector(client, DefaultSTSEndpoint, tt.awsEndpointURL, store)

			pod := &corev1.Pod{Spec: corev1.PodSpec{ServiceAccountName: tt.sa.Name}}
			req := admissionRequestForPod(t, pod)
			req.Namespace = tt.sa.Namespace

			opts, err := injector.injectionOptionsFor(t.Context(), req)
			if err != nil {
				t.Fatalf("injectionOptionsFor: %v", err)
			}

			if opts.stsEndpoint != tt.wantSTSEndpoint {
				t.Errorf("stsEndpoint = %q, want %q", opts.stsEndpoint, tt.wantSTSEndpoint)
			}

			if opts.injectPodIdentity != tt.wantPodIdentity {
				t.Errorf("injectPodIdentity = %v, want %v", opts.injectPodIdentity, tt.wantPodIdentity)
			}

			if opts.awsEndpointURL != tt.wantAWSEndpointURL {
				t.Errorf("awsEndpointURL = %q, want %q", opts.awsEndpointURL, tt.wantAWSEndpointURL)
			}
		})
	}
}

// TestInjectionOptionsFor_NilPodIdentityStore verifies that a nil
// podIdentityStore (Pod Identity support disabled) never selects Pod
// Identity injection, even when the ServiceAccount does not exist (fjord's
// own fail-open behavior for the IRSA gate).
func TestInjectionOptionsFor_NilPodIdentityStore(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	injector := NewInjector(client, DefaultSTSEndpoint, "", nil)

	pod := &corev1.Pod{Spec: corev1.PodSpec{ServiceAccountName: "missing-sa"}}
	req := admissionRequestForPod(t, pod)
	req.Namespace = "default"

	opts, err := injector.injectionOptionsFor(t.Context(), req)
	if err != nil {
		t.Fatalf("injectionOptionsFor: %v", err)
	}

	if opts != (injectionOptions{}) {
		t.Errorf("injectionOptionsFor() = %+v, want zero value", opts)
	}
}
