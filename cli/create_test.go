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

func TestValidateCreateClusterOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    *createClusterOptions
		goos    string
		wantErr bool
	}{
		{
			name: "linux is fine",
			opts: &createClusterOptions{enableAuth: true},
			goos: "linux",
		},
		{
			name: "darwin with auth disabled is fine",
			opts: &createClusterOptions{enableAuth: false},
			goos: "darwin",
		},
		{
			name:    "darwin with auth enabled is rejected",
			opts:    &createClusterOptions{enableAuth: true},
			goos:    "darwin",
			wantErr: true,
		},
		{
			name:    "--with-loadbalancer without --enable-auth is rejected",
			opts:    &createClusterOptions{enableAuth: false, withLoadBalancer: true},
			goos:    "linux",
			wantErr: true,
		},
		{
			name: "--with-loadbalancer with --enable-auth is fine",
			opts: &createClusterOptions{enableAuth: true, withLoadBalancer: true},
			goos: "linux",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateCreateClusterOptions(tt.opts, tt.goos)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateCreateClusterOptions() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestClusterReadyMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts *createClusterOptions
		want string
	}{
		{
			name: "auth enabled reports the fixed NodePort",
			opts: &createClusterOptions{name: "fjord", enableAuth: true},
			want: `Cluster "fjord" is ready. kubectl context is set to "fjord". fjord-agent's fake STS API is reachable at 127.0.0.1:30080.`,
		},
		{
			name: "auth disabled omits the endpoint",
			opts: &createClusterOptions{name: "fjord", enableAuth: false},
			want: `Cluster "fjord" is ready. kubectl context is set to "fjord".`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := clusterReadyMessage(tt.opts); got != tt.want {
				t.Errorf("clusterReadyMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
