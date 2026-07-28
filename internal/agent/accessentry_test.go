package agent

import (
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// accessEntryStoreConstructors holds the AccessEntryStore implementations
// every test in this file runs against.
var accessEntryStoreConstructors = map[string]func() AccessEntryStore{
	"InMemory":  func() AccessEntryStore { return NewInMemoryAccessEntryStore() },
	"ConfigMap": func() AccessEntryStore { return NewConfigMapAccessEntryStore(fake.NewClientset()) },
}

func TestAccessEntryStore_PutGet(t *testing.T) {
	t.Parallel()

	for name, newStore := range accessEntryStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()

			entry := AccessEntry{
				PrincipalARN:     eksAPITestPrincipalARN,
				Username:         "alice",
				KubernetesGroups: []string{"view-only"},
				Type:             AccessEntryTypeStandard,
			}

			if err := store.Put(ctx, &entry); err != nil {
				t.Fatalf("Put: %v", err)
			}

			got, err := store.Get(ctx, entry.PrincipalARN)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			if got.Username != entry.Username || got.PrincipalARN != entry.PrincipalARN {
				t.Errorf("Get() = %+v, want %+v", got, entry)
			}
		})
	}
}

func TestAccessEntryStore_GetNotFound(t *testing.T) {
	t.Parallel()

	for name, newStore := range accessEntryStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()

			_, err := store.Get(t.Context(), "arn:aws:iam::000000000000:user/ghost")
			if !errors.Is(err, ErrAccessEntryNotFound) {
				t.Errorf("Get() error = %v, want ErrAccessEntryNotFound", err)
			}
		})
	}
}

func TestAccessEntryStore_PutReplaces(t *testing.T) {
	t.Parallel()

	for name, newStore := range accessEntryStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()
			arn := eksAPITestPrincipalARN

			if err := store.Put(ctx, &AccessEntry{PrincipalARN: arn, Username: "alice"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if err := store.Put(ctx, &AccessEntry{PrincipalARN: arn, Username: "alice-renamed"}); err != nil {
				t.Fatalf("Put (replace): %v", err)
			}

			got, err := store.Get(ctx, arn)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			if got.Username != "alice-renamed" {
				t.Errorf("Username = %q, want %q", got.Username, "alice-renamed")
			}
		})
	}
}

func TestAccessEntryStore_List(t *testing.T) {
	t.Parallel()

	for name, newStore := range accessEntryStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()

			if err := store.Put(ctx, &AccessEntry{PrincipalARN: "arn:aws:iam::000000000000:user/bob"}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if err := store.Put(ctx, &AccessEntry{PrincipalARN: eksAPITestPrincipalARN}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			entries, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}

			if len(entries) != 2 {
				t.Fatalf("len(entries) = %d, want 2", len(entries))
			}

			if entries[0].PrincipalARN != eksAPITestPrincipalARN {
				t.Errorf("entries[0].PrincipalARN = %q, want alice first (sorted)", entries[0].PrincipalARN)
			}
		})
	}
}

func TestAccessEntryStore_Delete(t *testing.T) {
	t.Parallel()

	for name, newStore := range accessEntryStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()
			ctx := t.Context()
			arn := eksAPITestPrincipalARN

			if err := store.Put(ctx, &AccessEntry{PrincipalARN: arn}); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if err := store.Delete(ctx, arn); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			if _, err := store.Get(ctx, arn); !errors.Is(err, ErrAccessEntryNotFound) {
				t.Errorf("Get() after delete error = %v, want ErrAccessEntryNotFound", err)
			}
		})
	}
}

func TestAccessEntryStore_DeleteNotFound(t *testing.T) {
	t.Parallel()

	for name, newStore := range accessEntryStoreConstructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := newStore()

			err := store.Delete(t.Context(), "arn:aws:iam::000000000000:user/ghost")
			if !errors.Is(err, ErrAccessEntryNotFound) {
				t.Errorf("Delete() error = %v, want ErrAccessEntryNotFound", err)
			}
		})
	}
}

func TestAccessEntry_EffectiveUsername(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry AccessEntry
		want  string
	}{
		{
			name:  "explicit username",
			entry: AccessEntry{PrincipalARN: eksAPITestPrincipalARN, Username: "alice"},
			want:  "alice",
		},
		{
			name:  "defaults to principal arn",
			entry: AccessEntry{PrincipalARN: eksAPITestPrincipalARN},
			want:  eksAPITestPrincipalARN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.entry.EffectiveUsername(); got != tt.want {
				t.Errorf("EffectiveUsername() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccessEntry_EffectiveGroups(t *testing.T) {
	t.Parallel()

	arn := eksAPITestPrincipalARN

	t.Run("no associated policies returns configured groups unchanged", func(t *testing.T) {
		t.Parallel()

		entry := AccessEntry{PrincipalARN: arn, KubernetesGroups: []string{testCustomGroup}}

		got := entry.EffectiveGroups()
		if len(got) != 1 || got[0] != testCustomGroup {
			t.Errorf("EffectiveGroups() = %v, want [custom-group]", got)
		}
	})

	t.Run("associated policy adds the synthetic access-entry group", func(t *testing.T) {
		t.Parallel()

		entry := AccessEntry{
			PrincipalARN:       arn,
			KubernetesGroups:   []string{testCustomGroup},
			AssociatedPolicies: []AssociatedAccessPolicy{{PolicyARN: StandardAccessPolicyView, AccessScopeType: "cluster"}},
		}

		got := entry.EffectiveGroups()

		wantGroup := principalGroup(arn)
		if len(got) != 2 || got[0] != testCustomGroup || got[1] != wantGroup {
			t.Errorf("EffectiveGroups() = %v, want [custom-group %s]", got, wantGroup)
		}
	})
}

func TestPrincipalHash_Deterministic(t *testing.T) {
	t.Parallel()

	arn := eksAPITestPrincipalARN

	a := principalHash(arn)
	b := principalHash(arn)

	if a != b {
		t.Errorf("principalHash(%q) is not deterministic: %q != %q", arn, a, b)
	}

	if len(a) != 16 {
		t.Errorf("len(principalHash(...)) = %d, want 16", len(a))
	}

	if other := principalHash("arn:aws:iam::000000000000:user/bob"); other == a {
		t.Errorf("principalHash produced the same digest for two different ARNs: %q", a)
	}
}
