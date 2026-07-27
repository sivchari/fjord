package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestStores returns one PrincipalStore per backing implementation so
// every test case runs against both.
func newTestStores() map[string]PrincipalStore {
	return map[string]PrincipalStore{
		"in-memory": NewInMemoryPrincipalStore(),
		"secret":    NewSecretPrincipalStore(fake.NewClientset()),
	}
}

// TestSecretPrincipalStore_PutIntoNilDataSecret guards a real-cluster crash:
// the API server returns a freshly created empty Secret with a nil Data map,
// and Put must not assign into it. The fake clientset preserves empty maps, so
// this pre-seeds a Secret with nil Data to reproduce the server behavior.
func TestSecretPrincipalStore_PutIntoNilDataSecret(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PrincipalsSecretName,
			Namespace: SystemNamespace,
		},
		Data: nil,
	})
	store := NewSecretPrincipalStore(client)

	p := Principal{AccessKeyID: "AKIATEST00000000TEST", ARN: PrincipalARN("alice"), Name: "alice"}
	if err := store.Put(context.Background(), p); err != nil {
		t.Fatalf("Put into nil-Data secret: %v", err)
	}

	got, err := store.GetByName(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}

	if got.AccessKeyID != p.AccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", got.AccessKeyID, p.AccessKeyID)
	}
}

func TestPrincipalStore_PutAndGet(t *testing.T) {
	t.Parallel()

	for name, store := range newTestStores() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			principal := Principal{AccessKeyID: "AKIAABCDEFGHIJKLMNOP", ARN: PrincipalARN("alice"), Name: "alice"}

			if err := store.Put(ctx, principal); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			byKey, err := store.GetByAccessKeyID(ctx, principal.AccessKeyID)
			if err != nil {
				t.Fatalf("GetByAccessKeyID() error = %v", err)
			}

			if *byKey != principal {
				t.Errorf("GetByAccessKeyID() = %+v, want %+v", *byKey, principal)
			}

			byName, err := store.GetByName(ctx, principal.Name)
			if err != nil {
				t.Fatalf("GetByName() error = %v", err)
			}

			if *byName != principal {
				t.Errorf("GetByName() = %+v, want %+v", *byName, principal)
			}
		})
	}
}

func TestPrincipalStore_List(t *testing.T) {
	t.Parallel()

	for name, store := range newTestStores() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()

			empty, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			if len(empty) != 0 {
				t.Errorf("List() on empty store = %v, want empty", empty)
			}

			want := []Principal{
				{AccessKeyID: "AKIA1111111111111111", ARN: PrincipalARN("alice"), Name: "alice"},
				{AccessKeyID: "AKIA2222222222222222", ARN: PrincipalARN("bob"), Name: "bob"},
			}

			for _, p := range want {
				if err := store.Put(ctx, p); err != nil {
					t.Fatalf("Put(%q) error = %v", p.Name, err)
				}
			}

			got, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("List() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestPrincipalStore_DuplicateNameRotatesAccessKey(t *testing.T) {
	t.Parallel()

	for name, store := range newTestStores() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			first := Principal{AccessKeyID: "AKIAOLD00000000000001", ARN: PrincipalARN("alice"), Name: "alice"}
			second := Principal{AccessKeyID: "AKIANEW00000000000001", ARN: PrincipalARN("alice"), Name: "alice"}

			if err := store.Put(ctx, first); err != nil {
				t.Fatalf("Put(first) error = %v", err)
			}

			if err := store.Put(ctx, second); err != nil {
				t.Fatalf("Put(second) error = %v", err)
			}

			if _, err := store.GetByAccessKeyID(ctx, first.AccessKeyID); !errors.Is(err, ErrPrincipalNotFound) {
				t.Errorf("GetByAccessKeyID(old key) error = %v, want ErrPrincipalNotFound", err)
			}

			got, err := store.GetByAccessKeyID(ctx, second.AccessKeyID)
			if err != nil {
				t.Fatalf("GetByAccessKeyID(new key) error = %v", err)
			}

			if *got != second {
				t.Errorf("GetByAccessKeyID(new key) = %+v, want %+v", *got, second)
			}

			list, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			if len(list) != 1 {
				t.Errorf("List() = %+v, want exactly one entry", list)
			}
		})
	}
}

func TestPrincipalStore_Delete(t *testing.T) {
	t.Parallel()

	for name, store := range newTestStores() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			principal := Principal{AccessKeyID: "AKIADEL00000000000001", ARN: PrincipalARN("carol"), Name: "carol"}

			if err := store.Put(ctx, principal); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			if err := store.Delete(ctx, principal.Name); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}

			if _, err := store.GetByName(ctx, principal.Name); !errors.Is(err, ErrPrincipalNotFound) {
				t.Errorf("GetByName() after delete error = %v, want ErrPrincipalNotFound", err)
			}

			if _, err := store.GetByAccessKeyID(ctx, principal.AccessKeyID); !errors.Is(err, ErrPrincipalNotFound) {
				t.Errorf("GetByAccessKeyID() after delete error = %v, want ErrPrincipalNotFound", err)
			}
		})
	}
}

func TestPrincipalStore_NotFound(t *testing.T) {
	t.Parallel()

	for name, store := range newTestStores() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()

			if _, err := store.GetByAccessKeyID(ctx, "missing"); !errors.Is(err, ErrPrincipalNotFound) {
				t.Errorf("GetByAccessKeyID() error = %v, want ErrPrincipalNotFound", err)
			}

			if _, err := store.GetByName(ctx, "missing"); !errors.Is(err, ErrPrincipalNotFound) {
				t.Errorf("GetByName() error = %v, want ErrPrincipalNotFound", err)
			}

			if err := store.Delete(ctx, "missing"); !errors.Is(err, ErrPrincipalNotFound) {
				t.Errorf("Delete() error = %v, want ErrPrincipalNotFound", err)
			}
		})
	}
}

func TestPrincipalARN(t *testing.T) {
	t.Parallel()

	got := PrincipalARN("alice")
	want := "arn:aws:iam::" + AccountID + ":user/alice"

	if got != want {
		t.Errorf("PrincipalARN(%q) = %q, want %q", "alice", got, want)
	}
}

func TestGenerateAccessKeyID(t *testing.T) {
	t.Parallel()

	seen := map[string]struct{}{}

	for range 20 {
		id, err := GenerateAccessKeyID()
		if err != nil {
			t.Fatalf("GenerateAccessKeyID() error = %v", err)
		}

		if !strings.HasPrefix(id, "AKIA") {
			t.Errorf("GenerateAccessKeyID() = %q, want AKIA prefix", id)
		}

		if len(id) != len("AKIA")+16 {
			t.Errorf("GenerateAccessKeyID() length = %d, want %d", len(id), len("AKIA")+16)
		}

		if _, ok := seen[id]; ok {
			t.Errorf("GenerateAccessKeyID() produced duplicate %q", id)
		}

		seen[id] = struct{}{}
	}
}
