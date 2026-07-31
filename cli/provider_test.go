package cli

import "testing"

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
