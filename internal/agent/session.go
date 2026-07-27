package agent

import "sync"

// sessionStore maps the temporary access key IDs issued by
// AssumeRoleWithWebIdentity to the assumed-role identity they represent, so a
// later GetCallerIdentity signed with those credentials resolves to the role
// rather than the account root. It is in-memory and cleared on restart, which
// matches the ephemeral nature of local session credentials.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]callerIdentityResult
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]callerIdentityResult)}
}

// put records the identity behind a temporary access key ID.
func (s *sessionStore) put(accessKeyID string, identity callerIdentityResult) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions[accessKeyID] = identity
}

// get returns the identity for a temporary access key ID, if known.
func (s *sessionStore) get(accessKeyID string) (callerIdentityResult, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	identity, ok := s.sessions[accessKeyID]

	return identity, ok
}
