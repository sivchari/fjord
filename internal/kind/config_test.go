package kind_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/sivchari/fjord/internal/kind"
)

func TestConfig_ToV1Alpha4(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		config            kind.Config
		wantName          string
		wantPatchCount    int
		wantPatchContains []string
	}{
		{
			name: "without coredns override",
			config: kind.Config{
				Name: "fjord",
			},
			wantName:       "fjord",
			wantPatchCount: 0,
		},
		{
			name: "with coredns override",
			config: kind.Config{
				Name:                   "fjord",
				CoreDNSImageRepository: "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/coredns",
				CoreDNSImageTag:        "v1.11.4-eksbuild.2",
			},
			wantName:       "fjord",
			wantPatchCount: 1,
			wantPatchContains: []string{
				"apiVersion: kubeadm.k8s.io/v1beta4",
				"kind: ClusterConfiguration",
				"imageRepository: 602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/coredns",
				"imageTag: v1.11.4-eksbuild.2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.config.ToV1Alpha4()
			assertConfigConverted(t, got, tt.wantName, tt.wantPatchCount, tt.wantPatchContains)
		})
	}
}

func assertConfigConverted(t *testing.T, got *v1alpha4.Cluster, wantName string, wantPatchCount int, wantPatchContains []string) {
	t.Helper()

	if got.Name != wantName {
		t.Errorf("Name = %q, want %q", got.Name, wantName)
	}

	if len(got.KubeadmConfigPatches) != wantPatchCount {
		t.Fatalf("len(KubeadmConfigPatches) = %d, want %d", len(got.KubeadmConfigPatches), wantPatchCount)
	}

	if wantPatchCount == 0 {
		return
	}

	patch := got.KubeadmConfigPatches[0]

	for _, want := range wantPatchContains {
		if !strings.Contains(patch, want) {
			t.Errorf("patch = %q, want it to contain %q", patch, want)
		}
	}
}
