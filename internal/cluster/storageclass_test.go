package cluster

import (
	"context"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureDefaultStorageClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing []*storagev1.StorageClass
	}{
		{
			name: "kind standard exists as default",
			existing: []*storagev1.StorageClass{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "standard",
						Annotations: map[string]string{defaultClassAnnotation: defaultClassValue},
					},
					Provisioner: localPathProvisioner,
				},
			},
		},
		{
			name:     "no storage class exists yet",
			existing: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objs := make([]runtime.Object, 0, len(tt.existing))
			for _, sc := range tt.existing {
				objs = append(objs, sc)
			}

			client := fake.NewClientset(objs...)

			if err := EnsureDefaultStorageClass(context.Background(), client); err != nil {
				t.Fatalf("EnsureDefaultStorageClass: %v", err)
			}

			assertGP2IsOnlyDefault(t, client)
		})
	}
}

// assertGP2IsOnlyDefault verifies gp2 exists with the local-path provisioner
// and is the only default StorageClass.
func assertGP2IsOnlyDefault(t *testing.T, client kubernetes.Interface) {
	t.Helper()

	scs, err := client.StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list storage classes: %v", err)
	}

	var defaults []string

	for i := range scs.Items {
		if scs.Items[i].Annotations[defaultClassAnnotation] == defaultClassValue {
			defaults = append(defaults, scs.Items[i].Name)
		}
	}

	if len(defaults) != 1 || defaults[0] != "gp2" {
		t.Errorf("default storage classes = %v, want exactly [gp2]", defaults)
	}

	gp2, err := client.StorageV1().StorageClasses().Get(context.Background(), "gp2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get gp2: %v", err)
	}

	if gp2.Provisioner != localPathProvisioner {
		t.Errorf("gp2 provisioner = %q, want %q", gp2.Provisioner, localPathProvisioner)
	}

	// gp3 is common on real EKS clusters (haro uses it); it must exist and
	// bind locally, but must not be a second default.
	gp3, err := client.StorageV1().StorageClasses().Get(context.Background(), "gp3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get gp3: %v", err)
	}

	if gp3.Provisioner != localPathProvisioner {
		t.Errorf("gp3 provisioner = %q, want %q", gp3.Provisioner, localPathProvisioner)
	}

	if gp3.Annotations[defaultClassAnnotation] == defaultClassValue {
		t.Errorf("gp3 must not be a default StorageClass")
	}
}

func TestEnsureDefaultStorageClassIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	for range 2 {
		if err := EnsureDefaultStorageClass(context.Background(), client); err != nil {
			t.Fatalf("EnsureDefaultStorageClass: %v", err)
		}
	}

	scs, err := client.StorageV1().StorageClasses().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list storage classes: %v", err)
	}

	if len(scs.Items) != 2 {
		t.Errorf("found %d storage classes, want 2 (gp2, gp3)", len(scs.Items))
	}
}
