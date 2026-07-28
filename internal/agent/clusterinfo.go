package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ClusterInfoConfigMapName is the name of the ConfigMap DescribeCluster
	// reads its endpoint and certificate authority from.
	ClusterInfoConfigMapName = "fjord-cluster-info"

	clusterInfoConfigMapKey = "info"
)

// ErrClusterInfoNotFound is returned when no ClusterInfo has been
// registered yet.
var ErrClusterInfoNotFound = errors.New("agent: cluster info not found")

// ClusterInfo is the cluster endpoint and certificate authority
// DescribeCluster reports, populated by `fjord create cluster` from the
// kind-provisioned control plane's own kubeconfig.
type ClusterInfo struct {
	// Endpoint is the cluster's Kubernetes API server URL.
	Endpoint string `json:"endpoint"`
	// CertificateAuthorityData is the base64-encoded PEM certificate
	// authority data for Endpoint, matching the real EKS API's
	// cluster.certificateAuthority.data shape.
	CertificateAuthorityData string `json:"certificateAuthorityData"`
}

// ClusterInfoStore manages the single ClusterInfo record DescribeCluster
// serves.
type ClusterInfoStore interface {
	// Put registers info, replacing any previously registered ClusterInfo.
	Put(ctx context.Context, info ClusterInfo) error
	// Get returns the registered ClusterInfo, or ErrClusterInfoNotFound if
	// none has been registered yet.
	Get(ctx context.Context) (*ClusterInfo, error)
}

// ConfigMapClusterInfoStore is a ClusterInfoStore backed by the kube-system
// ConfigMap named ClusterInfoConfigMapName.
type ConfigMapClusterInfoStore struct {
	client kubernetes.Interface
}

var _ ClusterInfoStore = (*ConfigMapClusterInfoStore)(nil)

// NewConfigMapClusterInfoStore returns a ConfigMapClusterInfoStore using
// client to read and write the ClusterInfoConfigMapName ConfigMap.
func NewConfigMapClusterInfoStore(client kubernetes.Interface) *ConfigMapClusterInfoStore {
	return &ConfigMapClusterInfoStore{client: client}
}

// Put implements ClusterInfoStore.
func (s *ConfigMapClusterInfoStore) Put(ctx context.Context, info ClusterInfo) error {
	encoded, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal cluster info: %w", err)
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClusterInfoConfigMapName,
			Namespace: SystemNamespace,
		},
		Data: map[string]string{clusterInfoConfigMapKey: string(encoded)},
	}

	_, err = s.client.CoreV1().ConfigMaps(SystemNamespace).Create(ctx, configMap, metav1.CreateOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %q configmap: %w", ClusterInfoConfigMapName, err)
	}

	existing, err := s.client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, ClusterInfoConfigMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get %q configmap: %w", ClusterInfoConfigMapName, err)
	}

	existing.Data = configMap.Data

	if _, err := s.client.CoreV1().ConfigMaps(SystemNamespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %q configmap: %w", ClusterInfoConfigMapName, err)
	}

	return nil
}

// Get implements ClusterInfoStore.
func (s *ConfigMapClusterInfoStore) Get(ctx context.Context) (*ClusterInfo, error) {
	configMap, err := s.client.CoreV1().ConfigMaps(SystemNamespace).Get(ctx, ClusterInfoConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrClusterInfoNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get %q configmap: %w", ClusterInfoConfigMapName, err)
	}

	raw, ok := configMap.Data[clusterInfoConfigMapKey]
	if !ok {
		return nil, ErrClusterInfoNotFound
	}

	var info ClusterInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, fmt.Errorf("unmarshal cluster info: %w", err)
	}

	return &info, nil
}

// InMemoryClusterInfoStore is an in-memory ClusterInfoStore for tests.
type InMemoryClusterInfoStore struct {
	mu   sync.Mutex
	info *ClusterInfo
}

var _ ClusterInfoStore = (*InMemoryClusterInfoStore)(nil)

// NewInMemoryClusterInfoStore returns an empty InMemoryClusterInfoStore.
func NewInMemoryClusterInfoStore() *InMemoryClusterInfoStore {
	return &InMemoryClusterInfoStore{}
}

// Put implements ClusterInfoStore.
func (s *InMemoryClusterInfoStore) Put(_ context.Context, info ClusterInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.info = &info

	return nil
}

// Get implements ClusterInfoStore.
func (s *InMemoryClusterInfoStore) Get(_ context.Context) (*ClusterInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.info == nil {
		return nil, ErrClusterInfoNotFound
	}

	info := *s.info

	return &info, nil
}
