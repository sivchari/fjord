package cli

import "testing"

func TestPrincipalRegistryOptions_Context(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts principalRegistryOptions
		want string
	}{
		{
			name: "kind default derives kind-<name>",
			opts: principalRegistryOptions{clusterName: "fjord", provider: providerKind},
			want: "kind-fjord",
		},
		{
			name: "rask default derives <name>",
			opts: principalRegistryOptions{clusterName: "fjord", provider: providerRask},
			want: "fjord",
		},
		{
			name: "explicit --context overrides the provider default",
			opts: principalRegistryOptions{clusterName: "fjord", provider: providerRask, kubeContext: "my-context"},
			want: "my-context",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.opts.context(); got != tt.want {
				t.Errorf("context() = %q, want %q", got, tt.want)
			}
		})
	}
}
