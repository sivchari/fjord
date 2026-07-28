package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// AccessEntriesConfigMapName is the name of the ConfigMap backing the
	// EKS access entry registry.
	AccessEntriesConfigMapName = "fjord-access-entries"

	// accessEntryGroupPrefix prefixes the synthetic Kubernetes group
	// AssociateAccessPolicy binds a ClusterRoleBinding/RoleBinding to for a
	// principal's associated policies (see principalGroup).
	accessEntryGroupPrefix = "fjord:access-entry:"

	// AccessEntryTypeStandard is the only AccessEntry Type fjord's facade
	// creates, matching the real EKS API's default for IAM principals (as
	// opposed to EC2_LINUX/EC2_WINDOWS/FARGATE_LINUX, which fjord does not
	// emulate).
	AccessEntryTypeStandard = "STANDARD"
)

// ErrAccessEntryNotFound is returned when no AccessEntry is registered for a
// given principal ARN.
var ErrAccessEntryNotFound = errors.New("agent: access entry not found")

// AccessEntry binds an IAM principal ARN to the Kubernetes identity
// (username and groups) it authenticates as, mirroring an EKS access entry.
// KubernetesGroups holds only the groups CreateAccessEntry registered
// explicitly; the synthetic group AssociateAccessPolicy grants is computed
// on demand (see principalGroup and EffectiveGroups) rather than stored
// here, so associating/disassociating a policy never needs to rewrite it.
type AccessEntry struct {
	PrincipalARN       string                   `json:"principalArn"`
	Username           string                   `json:"username"`
	KubernetesGroups   []string                 `json:"kubernetesGroups,omitempty"`
	Type               string                   `json:"type"`
	AssociatedPolicies []AssociatedAccessPolicy `json:"associatedPolicies,omitempty"`
}

// AssociatedAccessPolicy is a single EKS access policy AssociateAccessPolicy
// bound to an AccessEntry.
type AssociatedAccessPolicy struct {
	PolicyARN       string   `json:"policyArn"`
	AccessScopeType string   `json:"accessScopeType"` // "cluster" or "namespace"
	Namespaces      []string `json:"namespaces,omitempty"`
}

// EffectiveUsername returns e's Kubernetes username, defaulting to its
// principal ARN when CreateAccessEntry did not set one explicitly, matching
// the real EKS API's own default.
func (e *AccessEntry) EffectiveUsername() string {
	if e.Username != "" {
		return e.Username
	}

	return e.PrincipalARN
}

// EffectiveGroups returns e's full Kubernetes group membership: its
// explicitly configured KubernetesGroups, plus the synthetic
// fjord:access-entry:<hash> group AssociateAccessPolicy's
// ClusterRoleBinding/RoleBinding subjects, if e has any associated policies.
func (e *AccessEntry) EffectiveGroups() []string {
	if len(e.AssociatedPolicies) == 0 {
		return e.KubernetesGroups
	}

	groups := make([]string, 0, len(e.KubernetesGroups)+1)
	groups = append(groups, e.KubernetesGroups...)

	return append(groups, principalGroup(e.PrincipalARN))
}

// principalHash returns a short, filesystem/ConfigMap-key-safe digest of
// arn, used both as the AccessEntryStore's ConfigMap key (principal ARNs
// contain ":" and "/", which ConfigMap Data keys may not) and as the
// synthetic access-entry group's unique suffix.
func principalHash(arn string) string {
	sum := sha256.Sum256([]byte(arn))

	return hex.EncodeToString(sum[:])[:16]
}

// principalGroup returns the synthetic Kubernetes group name
// AssociateAccessPolicy's ClusterRoleBinding/RoleBinding subjects for the
// principal identified by arn.
func principalGroup(arn string) string {
	return accessEntryGroupPrefix + principalHash(arn)
}

// AccessEntryStore manages the registry of EKS access entries.
type AccessEntryStore interface {
	// Put registers entry, keyed by its PrincipalARN. A later Put for the
	// same principal replaces it. entry is passed by pointer only to avoid
	// copying its several string/slice fields; implementations must not
	// retain or mutate it after Put returns.
	Put(ctx context.Context, entry *AccessEntry) error
	// Get returns the AccessEntry registered for principalARN, or
	// ErrAccessEntryNotFound.
	Get(ctx context.Context, principalARN string) (*AccessEntry, error)
	// List returns every registered AccessEntry, ordered by principal ARN.
	List(ctx context.Context) ([]AccessEntry, error)
	// Delete removes the AccessEntry registered for principalARN, or
	// returns ErrAccessEntryNotFound.
	Delete(ctx context.Context, principalARN string) error
}

// ConfigMapAccessEntryStore is an AccessEntryStore backed by the kube-system
// ConfigMap named AccessEntriesConfigMapName. Each entry is stored as a JSON
// blob keyed by principalHash(PrincipalARN). Writes use resourceVersion-based
// optimistic locking with a bounded conflict retry, matching
// ConfigMapPodIdentityStore.
type ConfigMapAccessEntryStore struct {
	client kubernetes.Interface
}

var _ AccessEntryStore = (*ConfigMapAccessEntryStore)(nil)

// NewConfigMapAccessEntryStore returns a ConfigMapAccessEntryStore using
// client to read and write the AccessEntriesConfigMapName ConfigMap.
func NewConfigMapAccessEntryStore(client kubernetes.Interface) *ConfigMapAccessEntryStore {
	return &ConfigMapAccessEntryStore{client: client}
}

// Put implements AccessEntryStore.
func (s *ConfigMapAccessEntryStore) Put(ctx context.Context, entry *AccessEntry) error {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal access entry %q: %w", entry.PrincipalARN, err)
	}

	key := principalHash(entry.PrincipalARN)

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := s.getOrCreateConfigMap(ctx)
		if err != nil {
			return err
		}

		configMap.Data[key] = string(encoded)

		return s.updateConfigMap(ctx, configMap)
	})
	if err != nil {
		return fmt.Errorf("put access entry %q: %w", entry.PrincipalARN, err)
	}

	return nil
}

// Get implements AccessEntryStore.
func (s *ConfigMapAccessEntryStore) Get(ctx context.Context, principalARN string) (*AccessEntry, error) {
	configMap, err := s.getConfigMap(ctx)
	if err != nil {
		return nil, err
	}

	raw, ok := configMap.Data[principalHash(principalARN)]
	if !ok {
		return nil, ErrAccessEntryNotFound
	}

	var entry AccessEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return nil, fmt.Errorf("unmarshal access entry %q: %w", principalARN, err)
	}

	return &entry, nil
}

// List implements AccessEntryStore.
func (s *ConfigMapAccessEntryStore) List(ctx context.Context) ([]AccessEntry, error) {
	entries, err := s.list(ctx)
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].PrincipalARN < entries[j].PrincipalARN })

	return entries, nil
}

// Delete implements AccessEntryStore.
func (s *ConfigMapAccessEntryStore) Delete(ctx context.Context, principalARN string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := s.getConfigMap(ctx)
		if err != nil {
			return err
		}

		key := principalHash(principalARN)
		if _, ok := configMap.Data[key]; !ok {
			return ErrAccessEntryNotFound
		}

		delete(configMap.Data, key)

		return s.updateConfigMap(ctx, configMap)
	})
	if err != nil {
		return fmt.Errorf("delete access entry %q: %w", principalARN, err)
	}

	return nil
}

// list decodes every access entry currently stored in the ConfigMap, in
// unspecified order. A missing ConfigMap is treated as an empty registry.
func (s *ConfigMapAccessEntryStore) list(ctx context.Context) ([]AccessEntry, error) {
	configMap, err := s.getConfigMap(ctx)
	if errors.Is(err, ErrAccessEntryNotFound) {
		return []AccessEntry{}, nil
	}

	if err != nil {
		return nil, err
	}

	entries := make([]AccessEntry, 0, len(configMap.Data))

	for key, raw := range configMap.Data {
		var entry AccessEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, fmt.Errorf("unmarshal access entry %q: %w", key, err)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// getConfigMap fetches the AccessEntriesConfigMapName ConfigMap. A missing
// ConfigMap is treated as an empty registry and reported via
// ErrAccessEntryNotFound.
func (s *ConfigMapAccessEntryStore) getConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	configMap, err := s.client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, AccessEntriesConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrAccessEntryNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get %q configmap: %w", AccessEntriesConfigMapName, err)
	}

	return configMap, nil
}

// getOrCreateConfigMap fetches the AccessEntriesConfigMapName ConfigMap,
// creating it with an empty Data map if it does not already exist.
func (s *ConfigMapAccessEntryStore) getOrCreateConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	configMap, err := s.getConfigMap(ctx)
	if errors.Is(err, ErrAccessEntryNotFound) {
		configMap, err = s.createConfigMap(ctx)
	}

	if err != nil {
		return nil, err
	}

	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}

	return configMap, nil
}

// createConfigMap creates the AccessEntriesConfigMapName ConfigMap with an
// empty Data map. A concurrent creator racing this call is not an error: the
// ConfigMap is fetched instead.
func (s *ConfigMapAccessEntryStore) createConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AccessEntriesConfigMapName,
			Namespace: SystemNamespace,
		},
		Data: map[string]string{},
	}

	created, err := s.client.CoreV1().ConfigMaps(SystemNamespace).Create(ctx, configMap, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return s.getConfigMap(ctx)
	}

	if err != nil {
		return nil, fmt.Errorf("create %q configmap: %w", AccessEntriesConfigMapName, err)
	}

	return created, nil
}

// updateConfigMap persists configMap via an Update call, relying on its
// resourceVersion for optimistic concurrency control.
func (s *ConfigMapAccessEntryStore) updateConfigMap(ctx context.Context, configMap *corev1.ConfigMap) error {
	if _, err := s.client.CoreV1().ConfigMaps(SystemNamespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %q configmap: %w", AccessEntriesConfigMapName, err)
	}

	return nil
}

// InMemoryAccessEntryStore is an in-memory AccessEntryStore for tests.
type InMemoryAccessEntryStore struct {
	mu      sync.Mutex
	entries map[string]AccessEntry // keyed by PrincipalARN
}

var _ AccessEntryStore = (*InMemoryAccessEntryStore)(nil)

// NewInMemoryAccessEntryStore returns an empty InMemoryAccessEntryStore.
func NewInMemoryAccessEntryStore() *InMemoryAccessEntryStore {
	return &InMemoryAccessEntryStore{entries: map[string]AccessEntry{}}
}

// Put implements AccessEntryStore.
func (s *InMemoryAccessEntryStore) Put(_ context.Context, entry *AccessEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[entry.PrincipalARN] = *entry

	return nil
}

// Get implements AccessEntryStore.
func (s *InMemoryAccessEntryStore) Get(_ context.Context, principalARN string) (*AccessEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[principalARN]
	if !ok {
		return nil, ErrAccessEntryNotFound
	}

	return &entry, nil
}

// List implements AccessEntryStore.
func (s *InMemoryAccessEntryStore) List(_ context.Context) ([]AccessEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make([]AccessEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].PrincipalARN < entries[j].PrincipalARN })

	return entries, nil
}

// Delete implements AccessEntryStore.
func (s *InMemoryAccessEntryStore) Delete(_ context.Context, principalARN string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.entries[principalARN]; !ok {
		return ErrAccessEntryNotFound
	}

	delete(s.entries, principalARN)

	return nil
}
