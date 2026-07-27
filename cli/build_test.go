package cli

import (
	"reflect"
	"testing"

	fjord "github.com/sivchari/fjord"
)

func TestAgentImageBuildArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		tag   string
		arch  string
		local bool
		want  []string
	}{
		{
			name:  "local build loads into the local docker daemon",
			tag:   "ghcr.io/sivchari/fjord/agent:v1",
			arch:  "amd64",
			local: true,
			want: []string{
				"build",
				"-f", "docker/Dockerfile",
				"--platform", "linux/amd64",
				"-t", "ghcr.io/sivchari/fjord/agent:v1",
				"--load",
				".",
			},
		},
		{
			name:  "non-local build pushes to the registry",
			tag:   "ghcr.io/sivchari/fjord/agent:v1",
			arch:  "arm64",
			local: false,
			want: []string{
				"build",
				"-f", "docker/Dockerfile",
				"--platform", "linux/arm64",
				"-t", "ghcr.io/sivchari/fjord/agent:v1",
				"--push",
				".",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := agentImageBuildArgs(tt.tag, tt.arch, tt.local)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("agentImageBuildArgs(%q, %q, %v) = %v, want %v", tt.tag, tt.arch, tt.local, got, tt.want)
			}
		})
	}
}

func TestDefaultAgentImage(t *testing.T) {
	t.Parallel()

	got := defaultAgentImage()

	want := agentImageRepository + ":" + fjord.Version
	if got != want {
		t.Errorf("defaultAgentImage() = %q, want %q", got, want)
	}
}
