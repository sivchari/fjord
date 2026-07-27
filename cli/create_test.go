package cli

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/sivchari/fjord/internal/agent"
)

func TestEnsureNodeRolePrincipal(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	ctx := t.Context()

	if err := ensureNodeRolePrincipal(ctx, client, "fjord-node-role"); err != nil {
		t.Fatalf("ensureNodeRolePrincipal() error = %v", err)
	}

	store := agent.NewSecretPrincipalStore(client)

	principal, err := store.GetByName(ctx, "fjord-node-role")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}

	wantARN := agent.RoleARN("fjord-node-role")
	if principal.ARN != wantARN {
		t.Errorf("principal ARN = %q, want %q", principal.ARN, wantARN)
	}
}

func TestEnsureNodeRolePrincipalIdempotent(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset()
	ctx := t.Context()

	if err := ensureNodeRolePrincipal(ctx, client, "fjord-node-role"); err != nil {
		t.Fatalf("ensureNodeRolePrincipal() error = %v", err)
	}

	store := agent.NewSecretPrincipalStore(client)

	first, err := store.GetByName(ctx, "fjord-node-role")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}

	if err := ensureNodeRolePrincipal(ctx, client, "fjord-node-role"); err != nil {
		t.Fatalf("ensureNodeRolePrincipal() second call error = %v", err)
	}

	second, err := store.GetByName(ctx, "fjord-node-role")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}

	if second.AccessKeyID != first.AccessKeyID {
		t.Errorf("second call access key = %q, want unchanged %q", second.AccessKeyID, first.AccessKeyID)
	}
}

func TestAgentHostPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		enableAuth bool
		hostPort   int32
		want       int32
	}{
		{
			name:       "auth enabled publishes the configured host port",
			enableAuth: true,
			hostPort:   48080,
			want:       48080,
		},
		{
			name:       "auth disabled publishes no port",
			enableAuth: false,
			hostPort:   48080,
			want:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := agentHostPort(tt.enableAuth, tt.hostPort)
			if got != tt.want {
				t.Errorf("agentHostPort(%v, %d) = %d, want %d", tt.enableAuth, tt.hostPort, got, tt.want)
			}
		})
	}
}
