package kind_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/sivchari/fjord/internal/kind"
)

type toV1Alpha4Test struct {
	name              string
	config            kind.Config
	wantName          string
	wantPatchCount    int
	wantPatchContains []string
}

var toV1Alpha4Tests = []toV1Alpha4Test{
	{
		name: "without coredns override",
		config: kind.Config{
			Name: "fjord",
		},
		wantName:       "fjord",
		wantPatchCount: 0,
	},
	{
		name: "with coredns override on 1.33 uses v1beta3",
		config: kind.Config{
			Name:                   "fjord",
			KubeVersion:            "v1.33.13",
			CoreDNSImageRepository: "public.ecr.aws/eks-distro/coredns",
			CoreDNSImageTag:        "v1.12.4-eks-1-33-29",
		},
		wantName:       "fjord",
		wantPatchCount: 1,
		wantPatchContains: []string{
			"apiVersion: kubeadm.k8s.io/v1beta3",
			"kind: ClusterConfiguration",
			"imageRepository: public.ecr.aws/eks-distro/coredns",
			"imageTag: v1.12.4-eks-1-33-29",
		},
	},
	{
		name: "with coredns override on 1.36 uses v1beta4",
		config: kind.Config{
			Name:                   "fjord",
			KubeVersion:            "v1.36.2",
			CoreDNSImageRepository: "public.ecr.aws/eks-distro/coredns",
			CoreDNSImageTag:        "v1.14.2-eks-1-36-5",
		},
		wantName:       "fjord",
		wantPatchCount: 1,
		wantPatchContains: []string{
			"apiVersion: kubeadm.k8s.io/v1beta4",
			"kind: ClusterConfiguration",
			"imageRepository: public.ecr.aws/eks-distro/coredns",
			"imageTag: v1.14.2-eks-1-36-5",
		},
	},
}

func TestConfig_ToV1Alpha4(t *testing.T) {
	t.Parallel()

	for _, tt := range toV1Alpha4Tests {
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
