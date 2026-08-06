package rask

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	raskcluster "github.com/sivchari/rask/pkg/cluster"

	clusterprovider "github.com/sivchari/fjord/internal/provider"
)

func TestWaitOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		waitForReady time.Duration
		want         string
	}{
		{
			name:         "zero disables waiting and maps to node",
			waitForReady: 0,
			want:         raskcluster.WaitNode,
		},
		{
			name:         "positive duration maps to coredns",
			waitForReady: 30 * time.Second,
			want:         raskcluster.WaitCoreDNS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := waitOption(tt.waitForReady); got != tt.want {
				t.Errorf("waitOption(%v) = %q, want %q", tt.waitForReady, got, tt.want)
			}
		})
	}
}

func TestCoreDNSImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		repository string
		tag        string
		want       string
		wantErr    bool
	}{
		{
			name: "both empty leaves rask's default",
			want: "",
		},
		{
			name:       "both set joins repository and tag",
			repository: "public.ecr.aws/eks-distro/coredns",
			tag:        "v1.12.4-eks-1-33-29",
			want:       "public.ecr.aws/eks-distro/coredns:v1.12.4-eks-1-33-29",
		},
		{
			name:       "repository set without tag errors",
			repository: "public.ecr.aws/eks-distro/coredns",
			wantErr:    true,
		},
		{
			name:    "tag set without repository errors",
			tag:     "v1.12.4-eks-1-33-29",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := coreDNSImage(&clusterprovider.Config{
				CoreDNSImageRepository: tt.repository,
				CoreDNSImageTag:        tt.tag,
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("coreDNSImage() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil {
				return
			}

			if got != tt.want {
				t.Errorf("coreDNSImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrebootFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o750); err != nil {
		t.Fatalf("mkdir nested dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("cert"), 0o600); err != nil {
		t.Fatalf("write tls.crt: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "nested", "webhook.yaml"), []byte("webhook"), 0o600); err != nil {
		t.Fatalf("write webhook.yaml: %v", err)
	}

	got, err := prebootFiles([]clusterprovider.Mount{
		{HostPath: dir, ContainerPath: "/etc/fjord/authn"},
	})
	if err != nil {
		t.Fatalf("prebootFiles() error = %v", err)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].Dest < got[j].Dest })

	want := []raskcluster.PrebootFile{
		{Src: filepath.Join(dir, "nested", "webhook.yaml"), Dest: "mount0/nested/webhook.yaml"},
		{Src: filepath.Join(dir, "tls.crt"), Dest: "mount0/tls.crt"},
	}

	if len(got) != len(want) {
		t.Fatalf("prebootFiles() = %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("prebootFiles()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPrebootFiles_MultipleMountsDisambiguated(t *testing.T) {
	t.Parallel()

	dirA, dirB := t.TempDir(), t.TempDir()

	if err := os.WriteFile(filepath.Join(dirA, "same.txt"), []byte("a"), 0o600); err != nil {
		t.Fatalf("write dirA/same.txt: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dirB, "same.txt"), []byte("b"), 0o600); err != nil {
		t.Fatalf("write dirB/same.txt: %v", err)
	}

	got, err := prebootFiles([]clusterprovider.Mount{
		{HostPath: dirA, ContainerPath: "/a"},
		{HostPath: dirB, ContainerPath: "/b"},
	})
	if err != nil {
		t.Fatalf("prebootFiles() error = %v", err)
	}

	dests := make(map[string]bool)
	for _, f := range got {
		dests[f.Dest] = true
	}

	for _, want := range []string{"mount0/same.txt", "mount1/same.txt"} {
		if !dests[want] {
			t.Errorf("prebootFiles() missing Dest %q, got %+v", want, got)
		}
	}
}

// authWebhookDestCase is one TestAuthWebhookDest case.
type authWebhookDestCase struct {
	name    string
	webhook *clusterprovider.AuthWebhook
	mounts  []clusterprovider.Mount
	want    string
	wantErr bool
}

// authWebhookDestCases returns TestAuthWebhookDest's table, split out so the
// test function itself stays short.
func authWebhookDestCases() []authWebhookDestCase {
	return []authWebhookDestCase{
		{
			name: "matching mount resolves to its preboot dest",
			webhook: &clusterprovider.AuthWebhook{
				ConfigFilePath:  "/etc/fjord/authn/webhook.yaml",
				VolumeMountPath: "/etc/fjord/authn",
			},
			mounts: []clusterprovider.Mount{
				{HostPath: "/host/authn", ContainerPath: "/etc/fjord/authn"},
			},
			want: "mount0/webhook.yaml",
		},
		{
			name: "second mount uses its own index prefix",
			webhook: &clusterprovider.AuthWebhook{
				ConfigFilePath:  "/etc/fjord/authn/webhook.yaml",
				VolumeMountPath: "/etc/fjord/authn",
			},
			mounts: []clusterprovider.Mount{
				{HostPath: "/host/other", ContainerPath: "/etc/fjord/other"},
				{HostPath: "/host/authn", ContainerPath: "/etc/fjord/authn"},
			},
			want: "mount1/webhook.yaml",
		},
		{
			name: "nested path under the mount keeps its subdirectories",
			webhook: &clusterprovider.AuthWebhook{
				ConfigFilePath:  "/etc/fjord/authn/sub/webhook.yaml",
				VolumeMountPath: "/etc/fjord/authn",
			},
			mounts: []clusterprovider.Mount{
				{HostPath: "/host/authn", ContainerPath: "/etc/fjord/authn"},
			},
			want: "mount0/sub/webhook.yaml",
		},
		{
			name: "no matching mount errors instead of guessing",
			webhook: &clusterprovider.AuthWebhook{
				ConfigFilePath:  "/etc/fjord/authn/webhook.yaml",
				VolumeMountPath: "/etc/fjord/authn",
			},
			mounts:  nil,
			wantErr: true,
		},
	}
}

func TestAuthWebhookDest(t *testing.T) {
	t.Parallel()

	for _, tt := range authWebhookDestCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := authWebhookDest(tt.webhook, tt.mounts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("authWebhookDest() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("authWebhookDest() = %q, want %q", got, tt.want)
			}
		})
	}
}
