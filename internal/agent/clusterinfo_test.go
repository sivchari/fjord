package agent

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var clusterInfoStoreConstructors = map[string]func() ClusterInfoStore{
	"InMemory":  func() ClusterInfoStore { return NewInMemoryClusterInfoStore() },
	"ConfigMap": func() ClusterInfoStore { return NewConfigMapClusterInfoStore(fake.NewClientset()) },
}

func TestClusterInfoStore_GetNotFound(t *testing.T) {
	t.Parallel()

	for name, newStore := range clusterInfoStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()

			_, err := store.Get(t.Context())
			if !errors.Is(err, ErrClusterInfoNotFound) {
				t.Errorf("Get() error = %v, want ErrClusterInfoNotFound", err)
			}
		})
	}
}

func TestClusterInfoStore_PutGet(t *testing.T) {
	t.Parallel()

	for name, newStore := range clusterInfoStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()

			info := ClusterInfo{
				Name:                     "my-cluster",
				Endpoint:                 "https://127.0.0.1:6443",
				CertificateAuthorityData: "ZmFrZS1jYQ==",
				Version:                  "1.33",
				PlatformVersion:          "eks.1",
			}

			if err := store.Put(ctx, &info); err != nil {
				t.Fatalf("Put: %v", err)
			}

			got, err := store.Get(ctx)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			if got.Name != info.Name || got.Endpoint != info.Endpoint || got.CertificateAuthorityData != info.CertificateAuthorityData ||
				got.Version != info.Version || got.PlatformVersion != info.PlatformVersion {
				t.Errorf("Get() = %+v, want %+v", got, info)
			}
		})
	}
}

// TestConfigMapClusterInfoStore_BackwardCompatible verifies Get tolerates a
// ConfigMap written before Name/Version/PlatformVersion existed, leaving
// them as their zero values instead of failing to unmarshal.
func TestConfigMapClusterInfoStore_BackwardCompatible(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterInfoConfigMapName, Namespace: SystemNamespace},
		Data: map[string]string{
			clusterInfoConfigMapKey: `{"endpoint":"https://127.0.0.1:6443","certificateAuthorityData":"ZmFrZS1jYQ=="}`,
		},
	}

	if _, err := client.CoreV1().ConfigMaps(SystemNamespace).Create(t.Context(), configMap, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create configmap: %v", err)
	}

	got, err := NewConfigMapClusterInfoStore(client).Get(t.Context())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Endpoint != "https://127.0.0.1:6443" || got.Name != "" || got.Version != "" || got.PlatformVersion != "" {
		t.Errorf("Get() = %+v, want endpoint preserved and name/version/platformVersion empty", got)
	}
}

func TestClusterInfoStore_PutReplaces(t *testing.T) {
	t.Parallel()

	for name, newStore := range clusterInfoStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()

			if err := store.Put(ctx, &ClusterInfo{Endpoint: "https://first"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if err := store.Put(ctx, &ClusterInfo{Endpoint: "https://second"}); err != nil {
				t.Fatalf("Put (replace): %v", err)
			}

			got, err := store.Get(ctx)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			if got.Endpoint != "https://second" {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, "https://second")
			}
		})
	}
}
