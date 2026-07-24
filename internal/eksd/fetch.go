package eksd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"
)

// DefaultBaseURL is the base URL of the EKS-D release manifest bucket.
const DefaultBaseURL = "https://distro.eks.amazonaws.com"

// manifest is the narrow subset of the EKS-D Release CRD manifest that
// eksd cares about.
type manifest struct {
	Spec struct {
		Number int `json:"number"`
	} `json:"spec"`
	Status struct {
		Components []component `json:"components"`
	} `json:"status"`
}

// component is a single entry of status.components in an EKS-D manifest.
type component struct {
	Name   string  `json:"name"`
	GitTag string  `json:"gitTag"`
	Assets []asset `json:"assets"`
}

// asset is a single entry of status.components[].assets in an EKS-D
// manifest.
type asset struct {
	Name    string   `json:"name"`
	Archive *archive `json:"archive,omitempty"`
	Image   *image   `json:"image,omitempty"`
}

// archive is the archive field of an EKS-D manifest asset.
type archive struct {
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

// image is the image field of an EKS-D manifest asset.
type image struct {
	URI string `json:"uri"`
}

// Fetch retrieves the EKS-D release manifest for channel (e.g. "1-33") and
// number (e.g. 20) from baseURL, and extracts the fields fjord needs.
func Fetch(ctx context.Context, client *http.Client, baseURL, channel string, number int) (*Release, error) {
	url := manifestURL(baseURL, channel, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d fetching manifest %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading manifest body %s: %w", url, err)
	}

	var m manifest
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest %s: %w", url, err)
	}

	return extractRelease(&m, channel)
}

// LatestNumber finds the highest EKS-D patch number published for channel
// under baseURL. It probes manifest URLs with an exponentially growing
// number, then binary searches the boundary between the last existing and
// first missing number.
func LatestNumber(ctx context.Context, client *http.Client, baseURL, channel string) (int, error) {
	ok, err := manifestExists(ctx, client, baseURL, channel, 1)
	if err != nil {
		return 0, err
	}

	if !ok {
		return 0, fmt.Errorf("no eks-d release found for channel %s", channel)
	}

	low, high := 1, 2

	for {
		ok, err := manifestExists(ctx, client, baseURL, channel, high)
		if err != nil {
			return 0, err
		}

		if !ok {
			break
		}

		low = high
		high *= 2
	}

	for low+1 < high {
		mid := (low + high) / 2

		ok, err := manifestExists(ctx, client, baseURL, channel, mid)
		if err != nil {
			return 0, err
		}

		if ok {
			low = mid
		} else {
			high = mid
		}
	}

	return low, nil
}

// manifestExists reports whether a manifest exists for channel/number under
// baseURL, using a HEAD request.
func manifestExists(ctx context.Context, client *http.Client, baseURL, channel string, number int) (bool, error) {
	url := manifestURL(baseURL, channel, number)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("building request for %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("probing manifest %s: %w", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusOK, nil
}

// manifestURL builds the EKS-D manifest URL for channel/number under
// baseURL.
func manifestURL(baseURL, channel string, number int) string {
	return fmt.Sprintf("%s/kubernetes-%s/kubernetes-%s-eks-%d.yaml", baseURL, channel, channel, number)
}

// extractRelease builds a Release from the parsed manifest m for channel.
func extractRelease(m *manifest, channel string) (*Release, error) {
	release := &Release{
		EKSVersion:    strings.ReplaceAll(channel, "-", "."),
		Channel:       channel,
		Number:        m.Spec.Number,
		ServerTarball: make(map[string]Asset),
	}

	for _, c := range m.Status.Components {
		switch c.Name {
		case "kubernetes":
			release.KubeVersion = c.GitTag

			for _, a := range c.Assets {
				arch, ok := serverTarballArch(a.Name)
				if !ok || a.Archive == nil {
					continue
				}

				release.ServerTarball[arch] = Asset{URI: a.Archive.URI, SHA256: a.Archive.SHA256}
			}
		case "coredns":
			if uri, ok := firstImageURI(c.Assets); ok {
				release.CoreDNSImage = uri
			}
		case "kube-proxy":
			if uri, ok := firstImageURI(c.Assets); ok {
				release.KubeProxyImage = uri
			}
		}
	}

	if err := release.validate(); err != nil {
		return nil, fmt.Errorf("extracting release for channel %s number %d: %w", channel, m.Spec.Number, err)
	}

	return release, nil
}

// validate reports whether r has every field extractRelease is expected to
// populate.
func (r *Release) validate() error {
	switch {
	case r.KubeVersion == "":
		return fmt.Errorf("kubernetes component not found")
	case len(r.ServerTarball) == 0:
		return fmt.Errorf("kubernetes-server tarball assets not found")
	case r.CoreDNSImage == "":
		return fmt.Errorf("coredns image not found")
	case r.KubeProxyImage == "":
		return fmt.Errorf("kube-proxy image not found")
	default:
		return nil
	}
}

// serverTarballArch reports the arch (e.g. "amd64") of a
// kubernetes-server-linux-<arch>.tar.gz asset name, or false if name does
// not match that pattern.
func serverTarballArch(name string) (string, bool) {
	const (
		prefix = "kubernetes-server-linux-"
		suffix = ".tar.gz"
	)

	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return "", false
	}

	return strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix), true
}

// firstImageURI returns the URI of the first Image-type asset in assets.
func firstImageURI(assets []asset) (string, bool) {
	for _, a := range assets {
		if a.Image != nil {
			return a.Image.URI, true
		}
	}

	return "", false
}
