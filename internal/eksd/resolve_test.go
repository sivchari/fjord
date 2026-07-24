package eksd_test

import (
	"errors"
	"testing"

	"github.com/sivchari/fjord/internal/eksd"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	for _, eksVersion := range eksd.SupportedVersions() {
		t.Run(eksVersion, func(t *testing.T) {
			t.Parallel()

			release, err := eksd.Resolve(eksVersion)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v, want nil", eksVersion, err)
			}

			checkRelease(t, release, eksVersion)
		})
	}
}

// checkRelease asserts that release has every field Resolve is expected to
// populate for eksVersion.
func checkRelease(t *testing.T, release *eksd.Release, eksVersion string) {
	t.Helper()

	if release.EKSVersion != eksVersion {
		t.Errorf("EKSVersion = %q, want %q", release.EKSVersion, eksVersion)
	}

	if release.Number <= 0 {
		t.Errorf("Number = %d, want > 0", release.Number)
	}

	if release.KubeVersion == "" {
		t.Error("KubeVersion is empty")
	}

	for _, arch := range []string{"amd64", "arm64"} {
		asset, ok := release.ServerTarball[arch]
		if !ok {
			t.Errorf("ServerTarball[%q] missing", arch)

			continue
		}

		if asset.URI == "" || asset.SHA256 == "" {
			t.Errorf("ServerTarball[%q] = %+v, want non-empty URI and SHA256", arch, asset)
		}
	}

	if release.CoreDNSImage == "" {
		t.Error("CoreDNSImage is empty")
	}

	if release.KubeProxyImage == "" {
		t.Error("KubeProxyImage is empty")
	}
}

func TestResolve_Unsupported(t *testing.T) {
	t.Parallel()

	_, err := eksd.Resolve("1.0")
	if err == nil {
		t.Fatal("Resolve(\"1.0\") error = nil, want non-nil")
	}

	var target *eksd.UnsupportedVersionError
	if !errors.As(err, &target) {
		t.Fatalf("Resolve(\"1.0\") error = %v, want *UnsupportedVersionError", err)
	}
}

func TestSupportedVersions(t *testing.T) {
	t.Parallel()

	versions := eksd.SupportedVersions()
	if len(versions) == 0 {
		t.Fatal("SupportedVersions() is empty")
	}

	for i := 1; i < len(versions); i++ {
		if versions[i-1] >= versions[i] {
			t.Errorf("SupportedVersions() not sorted: %q >= %q", versions[i-1], versions[i])
		}
	}
}
