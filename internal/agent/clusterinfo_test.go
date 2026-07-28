package agent

import (
	"errors"
	"testing"

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
				Endpoint:                 "https://127.0.0.1:6443",
				CertificateAuthorityData: "ZmFrZS1jYQ==",
			}

			if err := store.Put(ctx, info); err != nil {
				t.Fatalf("Put: %v", err)
			}

			got, err := store.Get(ctx)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			if got.Endpoint != info.Endpoint || got.CertificateAuthorityData != info.CertificateAuthorityData {
				t.Errorf("Get() = %+v, want %+v", got, info)
			}
		})
	}
}

func TestClusterInfoStore_PutReplaces(t *testing.T) {
	t.Parallel()

	for name, newStore := range clusterInfoStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()

			if err := store.Put(ctx, ClusterInfo{Endpoint: "https://first"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if err := store.Put(ctx, ClusterInfo{Endpoint: "https://second"}); err != nil {
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
