package agent

import "net/http"

// Server is the fjord-agent HTTP server backing fjord's fake AWS APIs. Its
// Handler mounts the fake STS endpoint at the request root; later phases
// mount the facade and EKS Pod Identity APIs onto additional paths on the
// same mux.
type Server struct {
	store    PrincipalStore
	sessions *sessionStore
}

// NewServer returns a Server that resolves principals via store.
func NewServer(store PrincipalStore) *Server {
	return &Server{store: store, sessions: newSessionStore()}
}

// Handler returns the http.Handler serving every API this Server mounts.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", s.dispatchSTS)

	return mux
}
