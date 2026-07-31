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
			name: "default derives the cluster name",
			opts: principalRegistryOptions{clusterName: "fjord"},
			want: "fjord",
		},
		{
			name: "explicit --context overrides the default",
			opts: principalRegistryOptions{clusterName: "fjord", kubeContext: "my-context"},
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
