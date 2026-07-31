package kind_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/sivchari/fjord/internal/kind"
	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

type toV1Alpha4Test struct {
	name              string
	config            clusterprovider.Config
	wantName          string
	wantPatchCount    int
	wantPatchContains []string
}

var toV1Alpha4Tests = []toV1Alpha4Test{
	{
		name: "without coredns override",
		config: clusterprovider.Config{
			Name: "fjord",
		},
		wantName:       "fjord",
		wantPatchCount: 0,
	},
	{
		name: "with coredns override on 1.33 uses v1beta3",
		config: clusterprovider.Config{
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
		config: clusterprovider.Config{
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
	{
		name: "auth webhook on 1.33 uses v1beta3 map extraArgs",
		config: clusterprovider.Config{
			Name:        "fjord",
			KubeVersion: "v1.33.13",
			AuthWebhook: &clusterprovider.AuthWebhook{
				ConfigFilePath:  "/etc/fjord/authn/webhook.yaml",
				VolumeName:      "fjord-authn",
				VolumeHostPath:  "/etc/fjord/authn",
				VolumeMountPath: "/etc/fjord/authn",
			},
		},
		wantName:       "fjord",
		wantPatchCount: 1,
		wantPatchContains: []string{
			"apiVersion: kubeadm.k8s.io/v1beta3",
			"apiServer:",
			"authentication-token-webhook-config-file: /etc/fjord/authn/webhook.yaml",
			"extraVolumes:",
			"name: fjord-authn",
			"hostPath: /etc/fjord/authn",
		},
	},
	{
		name: "auth webhook on 1.36 uses v1beta4 list extraArgs",
		config: clusterprovider.Config{
			Name:        "fjord",
			KubeVersion: "v1.36.2",
			AuthWebhook: &clusterprovider.AuthWebhook{
				ConfigFilePath:  "/etc/fjord/authn/webhook.yaml",
				VolumeName:      "fjord-authn",
				VolumeHostPath:  "/etc/fjord/authn",
				VolumeMountPath: "/etc/fjord/authn",
			},
		},
		wantName:       "fjord",
		wantPatchCount: 1,
		wantPatchContains: []string{
			"apiVersion: kubeadm.k8s.io/v1beta4",
			"- name: authentication-token-webhook-config-file",
			"value: /etc/fjord/authn/webhook.yaml",
		},
	},
}

func TestConfig_ToV1Alpha4(t *testing.T) {
	t.Parallel()

	for _, tt := range toV1Alpha4Tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := kind.ToV1Alpha4(&tt.config)
			assertConfigConverted(t, got, tt.wantName, tt.wantPatchCount, tt.wantPatchContains)
		})
	}
}

func TestConfig_ToV1Alpha4_HostPort(t *testing.T) {
	t.Parallel()

	t.Run("zero host port adds no port mapping", func(t *testing.T) {
		t.Parallel()

		got := kind.ToV1Alpha4(&clusterprovider.Config{Name: "fjord"})

		if len(got.Nodes) != 0 {
			t.Errorf("Nodes = %+v, want empty", got.Nodes)
		}
	})

	t.Run("nonzero host port maps the agent NodePort to the control-plane node", func(t *testing.T) {
		t.Parallel()

		got := kind.ToV1Alpha4(&clusterprovider.Config{Name: "fjord", HostPort: int32(48080)})

		if len(got.Nodes) != 1 {
			t.Fatalf("len(Nodes) = %d, want 1", len(got.Nodes))
		}

		node := got.Nodes[0]
		if node.Role != v1alpha4.ControlPlaneRole {
			t.Errorf("Role = %q, want %q", node.Role, v1alpha4.ControlPlaneRole)
		}

		if len(node.ExtraPortMappings) != 1 {
			t.Fatalf("len(ExtraPortMappings) = %d, want 1", len(node.ExtraPortMappings))
		}

		mapping := node.ExtraPortMappings[0]
		if mapping.ContainerPort != 30080 {
			t.Errorf("ContainerPort = %d, want 30080", mapping.ContainerPort)
		}

		if mapping.HostPort != 48080 {
			t.Errorf("HostPort = %d, want 48080", mapping.HostPort)
		}
	})
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
