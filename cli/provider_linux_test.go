package cli

import "testing"

// TestNewClusterProvider is linux-only by filename. On darwin the provider
// hands rask the rask-init binary embedded at build time
// (internal/rask/raskinit.go), and rask rejects the checked-in placeholder at
// construction — correctly, since booting a VM with it would otherwise fail
// minutes later as an opaque timeout. A fresh clone therefore cannot satisfy
// this on macOS without running `make rask-init` first, and a unit test
// should not depend on that. The darwin path is exercised by actually
// creating a cluster (see the macOS section of the README).
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
