package cli

import "testing"

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
