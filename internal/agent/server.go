package agent

import (
	"net/http"

	"k8s.io/client-go/kubernetes"
)

// Server is the fjord-agent HTTP server backing fjord's fake AWS APIs. Its
// Handler mounts the fake STS endpoint at the request root, and (when
// WithPodIdentity is passed to NewServer) the EKS Pod Identity
// assume-role-for-pod-identity endpoint and the pod-identity-associations
// facade endpoint.
type Server struct {
	store    PrincipalStore
	sessions *sessionStore

	// client is non-nil once any ServerOption needing an in-cluster
	// Kubernetes client (WithPodIdentity or WithEKSAPI) configured this
	// Server: it authenticates callers' ServiceAccount tokens via
	// TokenReview (podIdentityStore endpoints), and materializes RBAC for
	// AssociateAccessPolicy (accessEntryStore endpoints).
	client kubernetes.Interface

	// podIdentityStore is non-nil only when WithPodIdentity configured this
	// Server; it resolves an authenticated caller to a registered
	// PodIdentityAssociation.
	podIdentityStore PodIdentityStore

	// accessEntryStore and clusterInfoStore are non-nil only when
	// WithEKSAPI configured this Server, backing the access-entries and
	// ListClusters/DescribeCluster/addons facade endpoints respectively.
	accessEntryStore AccessEntryStore
	clusterInfoStore ClusterInfoStore
}

// ServerOption configures optional Server behavior.
type ServerOption func(*Server)

// WithPodIdentity enables Server's EKS Pod Identity endpoints
// (assume-role-for-pod-identity and the pod-identity-associations facade),
// using client to validate ServiceAccount tokens via TokenReview and store
// to resolve them to a registered PodIdentityAssociation.
func WithPodIdentity(client kubernetes.Interface, store PodIdentityStore) ServerOption {
	return func(s *Server) {
		s.client = client
		s.podIdentityStore = store
	}
}

// WithEKSAPI enables Server's EKS API facade endpoints (ListClusters,
// DescribeCluster, the addons family, and the access-entries family), using
// client to materialize RBAC for AssociateAccessPolicy, accessEntryStore to
// persist access entries, and clusterInfoStore to resolve
// ListClusters/DescribeCluster's cluster details.
func WithEKSAPI(client kubernetes.Interface, accessEntryStore AccessEntryStore, clusterInfoStore ClusterInfoStore) ServerOption {
	return func(s *Server) {
		s.client = client
		s.accessEntryStore = accessEntryStore
		s.clusterInfoStore = clusterInfoStore
	}
}

// NewServer returns a Server that resolves principals via store, configured
// by opts.
func NewServer(store PrincipalStore, opts ...ServerOption) *Server {
	s := &Server{store: store, sessions: newSessionStore()}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Handler returns the http.Handler serving every API this Server mounts.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", s.dispatchSTS)

	if s.podIdentityStore != nil {
		mux.HandleFunc("POST /clusters/{name}/assume-role-for-pod-identity", s.handleAssumeRoleForPodIdentity)
		mux.HandleFunc("POST /clusters/{name}/pod-identity-associations", s.handleCreatePodIdentityAssociation)
	}

	if s.accessEntryStore != nil {
		mux.HandleFunc("GET /clusters", s.handleListClusters)
		mux.HandleFunc("GET /clusters/{name}", s.handleDescribeCluster)
		mux.HandleFunc("GET /clusters/{name}/addons", s.handleListAddons)
		mux.HandleFunc("GET /clusters/{name}/addons/{addonName}", s.handleDescribeAddon)
		mux.HandleFunc("POST /clusters/{name}/access-entries", s.handleCreateAccessEntry)
		mux.HandleFunc("GET /clusters/{name}/access-entries", s.handleListAccessEntries)
		mux.HandleFunc("DELETE /clusters/{name}/access-entries", s.handleDeleteAccessEntry)
		mux.HandleFunc("POST /clusters/{name}/access-entries/access-policies", s.handleAssociateAccessPolicy)
		mux.HandleFunc("GET /clusters/{name}/access-entries/access-policies", s.handleListAssociatedAccessPolicies)
		mux.HandleFunc("DELETE /clusters/{name}/access-entries/access-policies", s.handleDisassociateAccessPolicy)
	}

	return mux
}
