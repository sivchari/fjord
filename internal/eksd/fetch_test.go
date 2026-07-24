package eksd_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"testing"

	"github.com/sivchari/fjord/internal/eksd"
)

// fixtureBody returns the contents of the canonical EKS-D 1-33/eks-20
// manifest fixture used by every test in this file.
func fixtureBody(t *testing.T) []byte {
	t.Helper()

	body, err := os.ReadFile("testdata/eksd-1-33-20.yaml")
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	return body
}

// newManifestServer starts an httptest.Server that serves the 1-33/eks-20
// fixture at its real manifest path and reports every other path as missing
// (mirroring the 403 the real distro.eks.amazonaws.com bucket returns for
// objects that do not exist).
func newManifestServer(t *testing.T) *httptest.Server {
	t.Helper()

	body := fixtureBody(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kubernetes-1-33/kubernetes-1-33-eks-20.yaml" {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		w.WriteHeader(http.StatusOK)

		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func TestFetch(t *testing.T) {
	t.Parallel()

	server := newManifestServer(t)

	got, err := eksd.Fetch(t.Context(), server.Client(), server.URL, "1-33", 20)
	if err != nil {
		t.Fatalf("Fetch() error = %v, want nil", err)
	}

	want := &eksd.Release{
		EKSVersion:  "1.33",
		Channel:     "1-33",
		Number:      20,
		KubeVersion: "v1.33.5",
		ServerTarball: map[string]eksd.Asset{
			"amd64": {
				URI:    "https://distro.eks.amazonaws.com/kubernetes-1-33/releases/20/artifacts/kubernetes/v1.33.5/kubernetes-server-linux-amd64.tar.gz",
				SHA256: "a9488722a64daedaf089de5a4afe8a17a4b0cf9a2c235ae95885b1876f618b08",
			},
			"arm64": {
				URI:    "https://distro.eks.amazonaws.com/kubernetes-1-33/releases/20/artifacts/kubernetes/v1.33.5/kubernetes-server-linux-arm64.tar.gz",
				SHA256: "9d2f5bc2169fdefe6c3f1045b36aa065e3183897ab756584ef3545e7f8d5a578",
			},
		},
		CoreDNSImage:   "public.ecr.aws/eks-distro/coredns/coredns:v1.12.4-eks-1-33-20",
		KubeProxyImage: "public.ecr.aws/eks-distro/kubernetes/kube-proxy:v1.33.5-eks-1-33-20",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fetch() = %+v, want %+v", got, want)
	}
}

func TestFetch_NotFound(t *testing.T) {
	t.Parallel()

	server := newManifestServer(t)

	_, err := eksd.Fetch(t.Context(), server.Client(), server.URL, "1-33", 999)
	if err == nil {
		t.Fatal("Fetch() error = nil, want non-nil")
	}
}

func TestLatestNumber(t *testing.T) {
	t.Parallel()

	body := fixtureBody(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var number int
		if _, err := fmt.Sscanf(path.Base(r.URL.Path), "kubernetes-1-33-eks-%d.yaml", &number); err != nil || number > 20 {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		w.WriteHeader(http.StatusOK)

		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	got, err := eksd.LatestNumber(t.Context(), server.Client(), server.URL, "1-33")
	if err != nil {
		t.Fatalf("LatestNumber() error = %v, want nil", err)
	}

	if got != 20 {
		t.Errorf("LatestNumber() = %d, want 20", got)
	}
}

func TestLatestNumber_NoReleases(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, err := eksd.LatestNumber(t.Context(), server.Client(), server.URL, "1-99")
	if err == nil {
		t.Fatal("LatestNumber() error = nil, want non-nil")
	}
}
