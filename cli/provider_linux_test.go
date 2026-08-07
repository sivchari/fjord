package cli

import "testing"

// TestNewClusterProvider is linux-only by filename. On darwin the provider
// hands rask the rask-init binary embedded at build time, and a checkout
// does not carry one (internal/rask/embedded/README.md), so construction
// fails there until `make rask-init` has run — a unit test should not
// depend on that. The darwin path is exercised by actually creating a
// cluster (see the macOS section of the README).
func TestNewClusterProvider(t *testing.T) {
	t.Parallel()

	got, err := newClusterProvider()
	if err != nil {
		t.Fatalf("newClusterProvider() error = %v", err)
	}

	if got == nil {
		t.Error("newClusterProvider() returned a nil provider")
	}
}
