package rask

import (
	"context"
	"strings"
	"testing"

	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

// TestProvider_CreateCluster_RejectsNodeImage checks that a NodeImage
// request is rejected before the zero-value provider ever touches its
// (here nil) inner rask Provider or logger, so this exercises the real
// CreateCluster method without needing a constructed rask Provider.
func TestProvider_CreateCluster_RejectsNodeImage(t *testing.T) {
	t.Parallel()

	p := &provider{}

	err := p.CreateCluster(context.Background(), "fjord", clusterprovider.CreateOptions{
		NodeImage: "kindest/node:v1.33.13",
	})
	if err == nil {
		t.Fatal("CreateCluster() error = nil, want an error rejecting NodeImage")
	}

	if !strings.Contains(err.Error(), "NodeImage") {
		t.Errorf("CreateCluster() error = %q, want it to mention NodeImage", err)
	}
}
