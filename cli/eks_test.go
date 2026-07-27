package cli

import "testing"

func TestLatestVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		versions []string
		want     string
	}{
		{
			name:     "picks highest minor numerically",
			versions: []string{"1.29", "1.30", "1.36", "1.9"},
			want:     "1.36",
		},
		{
			name:     "single version",
			versions: []string{"1.33"},
			want:     "1.33",
		},
		{
			name:     "unordered input",
			versions: []string{"1.35", "1.29", "1.34"},
			want:     "1.35",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := latestVersion(tt.versions)
			if got != tt.want {
				t.Errorf("latestVersion(%v) = %q, want %q", tt.versions, got, tt.want)
			}
		})
	}
}

func TestSplitImageRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ref      string
		wantRepo string
		wantTag  string
	}{
		{
			name:     "registry with tag",
			ref:      "public.ecr.aws/eks-distro/coredns/coredns:v1.12.0-eks-1-33-29",
			wantRepo: "public.ecr.aws/eks-distro/coredns/coredns",
			wantTag:  "v1.12.0-eks-1-33-29",
		},
		{
			name:     "registry with port and tag",
			ref:      "localhost:5000/coredns:v1.12.0",
			wantRepo: "localhost:5000/coredns",
			wantTag:  "v1.12.0",
		},
		{
			name:     "no tag",
			ref:      "public.ecr.aws/eks-distro/coredns/coredns",
			wantRepo: "public.ecr.aws/eks-distro/coredns/coredns",
			wantTag:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo, tag := splitImageRef(tt.ref)
			if repo != tt.wantRepo || tag != tt.wantTag {
				t.Errorf("splitImageRef(%q) = (%q, %q), want (%q, %q)", tt.ref, repo, tag, tt.wantRepo, tt.wantTag)
			}
		})
	}
}
