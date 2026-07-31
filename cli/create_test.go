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
			name: "kind on darwin is fine",
			opts: &createClusterOptions{provider: providerKind},
			goos: "darwin",
		},
		{
			name: "rask on linux is fine",
			opts: &createClusterOptions{provider: providerRask},
			goos: "linux",
		},
		{
			name:    "rask on darwin is rejected",
			opts:    &createClusterOptions{provider: providerRask},
			goos:    "darwin",
			wantErr: true,
		},
		{
			name:    "rask with --with-loadbalancer is rejected",
			opts:    &createClusterOptions{provider: providerRask, withLoadBalancer: true},
			goos:    "linux",
			wantErr: true,
		},
		{
			name: "kind with --with-loadbalancer is fine",
			opts: &createClusterOptions{provider: providerKind, withLoadBalancer: true},
			goos: "darwin",
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
			name: "kind with auth enabled",
			opts: &createClusterOptions{name: "fjord", provider: providerKind, enableAuth: true, hostPort: 48080},
			want: `Cluster "fjord" is ready. Set kubectl context to "kind-fjord". fjord-agent's fake STS API is reachable at localhost:48080.`,
		},
		{
			name: "kind with auth disabled omits the endpoint",
			opts: &createClusterOptions{name: "fjord", provider: providerKind, enableAuth: false},
			want: `Cluster "fjord" is ready. Set kubectl context to "kind-fjord".`,
		},
		{
			name: "rask with auth enabled reports the fixed NodePort",
			opts: &createClusterOptions{name: "fjord", provider: providerRask, enableAuth: true, hostPort: 48080},
			want: `Cluster "fjord" is ready. Set kubectl context to "fjord". fjord-agent's fake STS API is reachable at localhost:30080.`,
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
