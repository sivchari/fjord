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

	// client and podIdentityStore are non-nil only when WithPodIdentity
	// configured this Server: client authenticates callers' ServiceAccount
	// tokens via TokenReview, and podIdentityStore resolves them to a
	// registered PodIdentityAssociation.
	client           kubernetes.Interface
	podIdentityStore PodIdentityStore
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

	return mux
}
