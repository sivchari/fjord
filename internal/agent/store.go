// Package agent implements the server-side logic run by fjord-agent: the
// principal registry backing the fake STS's access-key-to-ARN resolution,
// and (in later phases) the IMDS, STS, and authenticator webhook servers.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// AccountID is the dummy AWS account ID fjord assigns to every
	// principal ARN it generates.
	AccountID = "000000000000"
	// PrincipalsSecretName is the name of the Secret backing the
	// principal registry.
	PrincipalsSecretName = "fjord-principals"
	// SystemNamespace is the namespace the principal registry Secret
	// lives in, matching where EKS system components run.
	SystemNamespace = "kube-system"

	accessKeyIDPrefix      = "AKIA"
	accessKeyIDSuffixChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	accessKeyIDSuffixLen   = 16
)

// ErrPrincipalNotFound is returned when a principal cannot be located by
// access key ID or name.
var ErrPrincipalNotFound = errors.New("agent: principal not found")

// Principal is an IAM principal fjord's fake STS resolves an AWS access key
// ID to.
type Principal struct {
	AccessKeyID string `json:"accessKeyId"`
	ARN         string `json:"arn"`
	Name        string `json:"name"`
}

// PrincipalStore manages the mapping from AWS access key IDs to IAM
// principal ARNs.
type PrincipalStore interface {
	// Put registers principal, keyed by its AccessKeyID. If another
	// principal with the same Name is already registered under a
	// different access key ID, that stale entry is removed so a name
	// resolves to exactly one access key ID.
	Put(ctx context.Context, principal Principal) error
	// GetByAccessKeyID returns the principal registered under
	// accessKeyID, or ErrPrincipalNotFound.
	GetByAccessKeyID(ctx context.Context, accessKeyID string) (*Principal, error)
	// GetByName returns the principal registered under name, or
	// ErrPrincipalNotFound.
	GetByName(ctx context.Context, name string) (*Principal, error)
	// List returns every registered principal, ordered by name.
	List(ctx context.Context) ([]Principal, error)
	// Delete removes the principal registered under name, or returns
	// ErrPrincipalNotFound.
	Delete(ctx context.Context, name string) error
}

// PrincipalARN returns the IAM user ARN fjord assigns to a principal named
// name.
func PrincipalARN(name string) string {
	return fmt.Sprintf("arn:aws:iam::%s:user/%s", AccountID, name)
}

// GenerateAccessKeyID returns a random AWS-access-key-ID-shaped string
// ("AKIA" followed by 16 uppercase alphanumeric characters), suitable as
// the registry key for a new Principal.
func GenerateAccessKeyID() (string, error) {
	suffix := make([]byte, accessKeyIDSuffixLen)

	for i := range suffix {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(accessKeyIDSuffixChars))))
		if err != nil {
			return "", fmt.Errorf("generate access key id: %w", err)
		}

		suffix[i] = accessKeyIDSuffixChars[n.Int64()]
	}

	return accessKeyIDPrefix + string(suffix), nil
}

// SecretPrincipalStore is a PrincipalStore backed by the kube-system
// Secret named fjord-principals. Each principal is stored as a JSON blob
// keyed by its access key ID. Writes use resourceVersion-based optimistic
// locking with a bounded conflict retry.
type SecretPrincipalStore struct {
	client kubernetes.Interface
}

var _ PrincipalStore = (*SecretPrincipalStore)(nil)

// NewSecretPrincipalStore returns a SecretPrincipalStore using client to
// read and write the fjord-principals Secret.
func NewSecretPrincipalStore(client kubernetes.Interface) *SecretPrincipalStore {
	return &SecretPrincipalStore{client: client}
}

// Put implements PrincipalStore.
func (s *SecretPrincipalStore) Put(ctx context.Context, principal Principal) error {
	encoded, err := json.Marshal(principal)
	if err != nil {
		return fmt.Errorf("marshal principal %q: %w", principal.Name, err)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := s.getOrCreateSecret(ctx)
		if err != nil {
			return err
		}

		for key, raw := range secret.Data {
			if key == principal.AccessKeyID {
				continue
			}

			var existing Principal
			if err := json.Unmarshal(raw, &existing); err == nil && existing.Name == principal.Name {
				delete(secret.Data, key)
			}
		}

		secret.Data[principal.AccessKeyID] = encoded

		return s.updateSecret(ctx, secret)
	})
	if err != nil {
		return fmt.Errorf("put principal %q: %w", principal.Name, err)
	}

	return nil
}

// GetByAccessKeyID implements PrincipalStore.
func (s *SecretPrincipalStore) GetByAccessKeyID(ctx context.Context, accessKeyID string) (*Principal, error) {
	secret, err := s.getSecret(ctx)
	if err != nil {
		return nil, err
	}

	raw, ok := secret.Data[accessKeyID]
	if !ok {
		return nil, ErrPrincipalNotFound
	}

	var principal Principal
	if err := json.Unmarshal(raw, &principal); err != nil {
		return nil, fmt.Errorf("unmarshal principal %q: %w", accessKeyID, err)
	}

	return &principal, nil
}

// GetByName implements PrincipalStore.
func (s *SecretPrincipalStore) GetByName(ctx context.Context, name string) (*Principal, error) {
	principals, err := s.list(ctx)
	if err != nil {
		return nil, err
	}

	for _, principal := range principals {
		if principal.Name == name {
			return &principal, nil
		}
	}

	return nil, ErrPrincipalNotFound
}

// List implements PrincipalStore.
func (s *SecretPrincipalStore) List(ctx context.Context) ([]Principal, error) {
	principals, err := s.list(ctx)
	if err != nil {
		return nil, err
	}

	sort.Slice(principals, func(i, j int) bool { return principals[i].Name < principals[j].Name })

	return principals, nil
}

// Delete implements PrincipalStore.
func (s *SecretPrincipalStore) Delete(ctx context.Context, name string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := s.getSecret(ctx)
		if err != nil {
			return err
		}

		key, err := findAccessKeyIDByName(secret, name)
		if err != nil {
			return err
		}

		delete(secret.Data, key)

		return s.updateSecret(ctx, secret)
	})
	if err != nil {
		return fmt.Errorf("delete principal %q: %w", name, err)
	}

	return nil
}

// list decodes every principal currently stored in the Secret, in
// unspecified order. A missing Secret is treated as an empty registry.
func (s *SecretPrincipalStore) list(ctx context.Context) ([]Principal, error) {
	secret, err := s.getSecret(ctx)
	if errors.Is(err, ErrPrincipalNotFound) {
		return []Principal{}, nil
	}

	if err != nil {
		return nil, err
	}

	principals := make([]Principal, 0, len(secret.Data))

	for accessKeyID, raw := range secret.Data {
		var principal Principal
		if err := json.Unmarshal(raw, &principal); err != nil {
			return nil, fmt.Errorf("unmarshal principal %q: %w", accessKeyID, err)
		}

		principals = append(principals, principal)
	}

	return principals, nil
}

// findAccessKeyIDByName returns the access key ID under which name is
// stored in secret, or ErrPrincipalNotFound.
func findAccessKeyIDByName(secret *corev1.Secret, name string) (string, error) {
	for accessKeyID, raw := range secret.Data {
		var principal Principal
		if err := json.Unmarshal(raw, &principal); err != nil {
			return "", fmt.Errorf("unmarshal principal %q: %w", accessKeyID, err)
		}

		if principal.Name == name {
			return accessKeyID, nil
		}
	}

	return "", ErrPrincipalNotFound
}

// getSecret fetches the fjord-principals Secret. A missing Secret is
// treated as an empty registry and reported via ErrPrincipalNotFound.
func (s *SecretPrincipalStore) getSecret(ctx context.Context) (*corev1.Secret, error) {
	secret, err := s.client.CoreV1().Secrets(SystemNamespace).Get(ctx, PrincipalsSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrPrincipalNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get %q secret: %w", PrincipalsSecretName, err)
	}

	return secret, nil
}

// getOrCreateSecret fetches the fjord-principals Secret, creating it with
// an empty Data map if it does not already exist.
func (s *SecretPrincipalStore) getOrCreateSecret(ctx context.Context) (*corev1.Secret, error) {
	secret, err := s.getSecret(ctx)
	if errors.Is(err, ErrPrincipalNotFound) {
		return s.createSecret(ctx)
	}

	if err != nil {
		return nil, err
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}

	return secret, nil
}

// createSecret creates the fjord-principals Secret with an empty Data map.
// A concurrent creator racing this call is not an error: the Secret is
// fetched instead.
func (s *SecretPrincipalStore) createSecret(ctx context.Context) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PrincipalsSecretName,
			Namespace: SystemNamespace,
		},
		Data: map[string][]byte{},
	}

	created, err := s.client.CoreV1().Secrets(SystemNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return s.getSecret(ctx)
	}

	if err != nil {
		return nil, fmt.Errorf("create %q secret: %w", PrincipalsSecretName, err)
	}

	return created, nil
}

// updateSecret persists secret via an Update call, relying on its
// resourceVersion for optimistic concurrency control.
func (s *SecretPrincipalStore) updateSecret(ctx context.Context, secret *corev1.Secret) error {
	if _, err := s.client.CoreV1().Secrets(SystemNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %q secret: %w", PrincipalsSecretName, err)
	}

	return nil
}

// InMemoryPrincipalStore is an in-memory PrincipalStore for tests.
type InMemoryPrincipalStore struct {
	mu         sync.Mutex
	principals map[string]Principal // keyed by access key ID
}

var _ PrincipalStore = (*InMemoryPrincipalStore)(nil)

// NewInMemoryPrincipalStore returns an empty InMemoryPrincipalStore.
func NewInMemoryPrincipalStore() *InMemoryPrincipalStore {
	return &InMemoryPrincipalStore{principals: map[string]Principal{}}
}

// Put implements PrincipalStore.
func (s *InMemoryPrincipalStore) Put(_ context.Context, principal Principal) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for accessKeyID, existing := range s.principals {
		if accessKeyID != principal.AccessKeyID && existing.Name == principal.Name {
			delete(s.principals, accessKeyID)
		}
	}

	s.principals[principal.AccessKeyID] = principal

	return nil
}

// GetByAccessKeyID implements PrincipalStore.
func (s *InMemoryPrincipalStore) GetByAccessKeyID(_ context.Context, accessKeyID string) (*Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	principal, ok := s.principals[accessKeyID]
	if !ok {
		return nil, ErrPrincipalNotFound
	}

	return &principal, nil
}

// GetByName implements PrincipalStore.
func (s *InMemoryPrincipalStore) GetByName(_ context.Context, name string) (*Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, principal := range s.principals {
		if principal.Name == name {
			return &principal, nil
		}
	}

	return nil, ErrPrincipalNotFound
}

// List implements PrincipalStore.
func (s *InMemoryPrincipalStore) List(_ context.Context) ([]Principal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	principals := make([]Principal, 0, len(s.principals))
	for _, principal := range s.principals {
		principals = append(principals, principal)
	}

	sort.Slice(principals, func(i, j int) bool { return principals[i].Name < principals[j].Name })

	return principals, nil
}

// Delete implements PrincipalStore.
func (s *InMemoryPrincipalStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for accessKeyID, principal := range s.principals {
		if principal.Name == name {
			delete(s.principals, accessKeyID)

			return nil
		}
	}

	return ErrPrincipalNotFound
}
