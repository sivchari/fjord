package cli

import (
	"errors"
	"io"
	"testing"

	"github.com/sivchari/fjord/internal/cluster"
	stderrlog "github.com/sivchari/fjord/internal/logger"
)

func TestNewClusterProvider(t *testing.T) {
	t.Parallel()

	log := stderrlog.NewStderr(io.Discard, 0)

	tests := []struct {
		name     string
		provider string
		wantErr  bool
	}{
		{
			name:     "kind",
			provider: providerKind,
		},
		{
			name:     "rask",
			provider: providerRask,
		},
		{
			name:     "unknown",
			provider: "docker-compose",
			wantErr:  true,
		},
		{
			name:     "empty",
			provider: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := newClusterProvider(tt.provider, log)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newClusterProvider(%q) error = %v, wantErr %v", tt.provider, err, tt.wantErr)
			}

			if tt.wantErr {
				if !errors.Is(err, errUnknownProvider) {
					t.Errorf("newClusterProvider(%q) error = %v, want wrapping errUnknownProvider", tt.provider, err)
				}

				return
			}

			if got == nil {
				t.Errorf("newClusterProvider(%q) returned a nil provider", tt.provider)
			}
		})
	}
}

func TestClusterContextName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		cluster  string
		want     string
	}{
		{
			name:     "kind prefixes kind-",
			provider: providerKind,
			cluster:  "fjord",
			want:     "kind-fjord",
		},
		{
			name:     "rask uses the cluster name verbatim",
			provider: providerRask,
			cluster:  "fjord",
			want:     "fjord",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := clusterContextName(tt.provider, tt.cluster); got != tt.want {
				t.Errorf("clusterContextName(%q, %q) = %q, want %q", tt.provider, tt.cluster, got, tt.want)
			}
		})
	}
}

func TestResolveAgentHostPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		flagChanged bool
		provider    string
		hostPort    int32
		want        int32
	}{
		{
			name:        "kind uses the requested (or default) host port",
			flagChanged: false,
			provider:    providerKind,
			hostPort:    defaultAgentHostPort,
			want:        defaultAgentHostPort,
		},
		{
			name:        "rask defaults to the fixed NodePort",
			flagChanged: false,
			provider:    providerRask,
			hostPort:    defaultAgentHostPort,
			want:        cluster.AgentNodePort,
		},
		{
			name:        "rask honors an explicit override",
			flagChanged: true,
			provider:    providerRask,
			hostPort:    12345,
			want:        12345,
		},
		{
			name:        "kind honors an explicit override",
			flagChanged: true,
			provider:    providerKind,
			hostPort:    12345,
			want:        12345,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveAgentHostPort(tt.flagChanged, tt.provider, tt.hostPort)
			if got != tt.want {
				t.Errorf("resolveAgentHostPort(%v, %q, %d) = %d, want %d", tt.flagChanged, tt.provider, tt.hostPort, got, tt.want)
			}
		})
	}
}
