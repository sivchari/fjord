package imagetar_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/sivchari/fjord/internal/imagetar"
)

// tarMember describes a single tar entry used to build fixture archives.
type tarMember struct {
	name string
	body []byte
}

const (
	manifestJSON = `[{"Config":"blobs/sha256/aaaa","RepoTags":["public.ecr.aws/eks-distro/kubernetes/kube-apiserver:v1.33.5-eks-1-33-20"],"Layers":["blobs/sha256/bbbb"]}]`

	indexJSON = `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:aaaa","annotations":{"io.containerd.image.name":"public.ecr.aws/eks-distro/kubernetes/kube-apiserver:v1.33.5-eks-1-33-20","org.opencontainers.image.ref.name":"public.ecr.aws/eks-distro/kubernetes/kube-apiserver:v1.33.5-eks-1-33-20"}}]}`

	ociLayout = `{"imageLayoutVersion":"1.0.0"}`
)

func fixtureMembers() []tarMember {
	return []tarMember{
		{name: "manifest.json", body: []byte(manifestJSON)},
		{name: "index.json", body: []byte(indexJSON)},
		{name: "oci-layout", body: []byte(ociLayout)},
		{name: "blobs/sha256/aaaa", body: []byte("config-blob-content")},
		{name: "blobs/sha256/bbbb", body: []byte("\x00\x01layer-blob-binary\xff\xfe")},
	}
}

func buildTar(t *testing.T, members []tarMember) []byte {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, m := range members {
		header := &tar.Header{
			Name: m.name,
			Mode: 0o644,
			Size: int64(len(m.body)),
		}

		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("WriteHeader(%s): %v", m.name, err)
		}

		if _, err := tw.Write(m.body); err != nil {
			t.Fatalf("Write(%s): %v", m.name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return buf.Bytes()
}

// readTar parses a tar byte slice into an ordered list of members.
func readTar(t *testing.T, data []byte) []tarMember {
	t.Helper()

	tr := tar.NewReader(bytes.NewReader(data))

	var got []tarMember

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("Next: %v", err)
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", header.Name, err)
		}

		got = append(got, tarMember{name: header.Name, body: body})
	}

	return got
}

func TestRewriteTags_RewritesManifestAndIndex(t *testing.T) {
	src := buildTar(t, fixtureMembers())

	replacements := map[string]string{
		"public.ecr.aws/eks-distro/kubernetes/kube-apiserver": "registry.k8s.io/kube-apiserver",
	}

	var dst bytes.Buffer

	if err := imagetar.RewriteTags(&dst, bytes.NewReader(src), replacements); err != nil {
		t.Fatalf("RewriteTags: %v", err)
	}

	got := readTar(t, dst.Bytes())

	manifest := findMember(t, got, "manifest.json")
	if bytes.Contains(manifest.body, []byte("public.ecr.aws")) {
		t.Errorf("manifest.json still contains old prefix: %s", manifest.body)
	}

	if !bytes.Contains(manifest.body, []byte(`"registry.k8s.io/kube-apiserver:v1.33.5-eks-1-33-20"`)) {
		t.Errorf("manifest.json missing rewritten RepoTag: %s", manifest.body)
	}

	index := findMember(t, got, "index.json")
	if bytes.Contains(index.body, []byte("public.ecr.aws")) {
		t.Errorf("index.json still contains old prefix: %s", index.body)
	}

	wantAnnotation := `"registry.k8s.io/kube-apiserver:v1.33.5-eks-1-33-20"`
	if got := bytes.Count(index.body, []byte(wantAnnotation)); got != 2 {
		t.Errorf("index.json annotation rewrite count = %d, want 2: %s", got, index.body)
	}
}

func TestRewriteTags_LeavesBlobsByteForByteUnchanged(t *testing.T) {
	members := fixtureMembers()
	src := buildTar(t, members)

	replacements := map[string]string{
		"public.ecr.aws/eks-distro/kubernetes/kube-apiserver": "registry.k8s.io/kube-apiserver",
	}

	var dst bytes.Buffer

	if err := imagetar.RewriteTags(&dst, bytes.NewReader(src), replacements); err != nil {
		t.Fatalf("RewriteTags: %v", err)
	}

	got := readTar(t, dst.Bytes())

	for _, name := range []string{"blobs/sha256/aaaa", "blobs/sha256/bbbb", "oci-layout"} {
		want := findMember(t, members, name)
		have := findMember(t, got, name)

		if !bytes.Equal(want.body, have.body) {
			t.Errorf("member %s body changed: want %q, got %q", name, want.body, have.body)
		}
	}
}

func TestRewriteTags_NoMatchingPrefixLeavesContentUnchanged(t *testing.T) {
	members := fixtureMembers()
	src := buildTar(t, members)

	replacements := map[string]string{
		"no.such.registry/never-matches": "unused.example/never-used",
	}

	var dst bytes.Buffer

	if err := imagetar.RewriteTags(&dst, bytes.NewReader(src), replacements); err != nil {
		t.Fatalf("RewriteTags: %v", err)
	}

	got := readTar(t, dst.Bytes())

	if len(got) != len(members) {
		t.Fatalf("member count = %d, want %d", len(got), len(members))
	}

	for i, want := range members {
		have := got[i]

		if have.name != want.name {
			t.Errorf("member[%d] name = %s, want %s", i, have.name, want.name)
		}

		if !bytes.Equal(have.body, want.body) {
			t.Errorf("member[%d] (%s) body changed: want %q, got %q", i, want.name, want.body, have.body)
		}
	}
}

func TestRewriteTags_PreservesMemberOrder(t *testing.T) {
	members := fixtureMembers()
	src := buildTar(t, members)

	replacements := map[string]string{
		"public.ecr.aws/eks-distro/kubernetes/kube-apiserver": "registry.k8s.io/kube-apiserver",
	}

	var dst bytes.Buffer

	if err := imagetar.RewriteTags(&dst, bytes.NewReader(src), replacements); err != nil {
		t.Fatalf("RewriteTags: %v", err)
	}

	got := readTar(t, dst.Bytes())

	if len(got) != len(members) {
		t.Fatalf("member count = %d, want %d", len(got), len(members))
	}

	for i, want := range members {
		if got[i].name != want.name {
			t.Errorf("member[%d] name = %s, want %s", i, got[i].name, want.name)
		}
	}
}

func TestRewriteTags_AppliesMultipleReplacements(t *testing.T) {
	manifest := `[{"RepoTags":["public.ecr.aws/eks-distro/kubernetes/kube-apiserver:v1.33.5-eks-1-33-20"]},{"RepoTags":["public.ecr.aws/eks-distro/kubernetes/kube-scheduler:v1.33.5-eks-1-33-20"]}]`

	src := buildTar(t, []tarMember{
		{name: "manifest.json", body: []byte(manifest)},
	})

	replacements := map[string]string{
		"public.ecr.aws/eks-distro/kubernetes/kube-apiserver": "registry.k8s.io/kube-apiserver",
		"public.ecr.aws/eks-distro/kubernetes/kube-scheduler": "registry.k8s.io/kube-scheduler",
	}

	var dst bytes.Buffer

	if err := imagetar.RewriteTags(&dst, bytes.NewReader(src), replacements); err != nil {
		t.Fatalf("RewriteTags: %v", err)
	}

	got := readTar(t, dst.Bytes())
	rewritten := findMember(t, got, "manifest.json")

	want := `[{"RepoTags":["registry.k8s.io/kube-apiserver:v1.33.5-eks-1-33-20"]},{"RepoTags":["registry.k8s.io/kube-scheduler:v1.33.5-eks-1-33-20"]}]`
	if string(rewritten.body) != want {
		t.Errorf("manifest.json = %s, want %s", rewritten.body, want)
	}
}

func findMember(t *testing.T, members []tarMember, name string) tarMember {
	t.Helper()

	for _, m := range members {
		if m.name == name {
			return m
		}
	}

	t.Fatalf("member %s not found", name)

	return tarMember{}
}
