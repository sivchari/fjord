package nodeimage_test

import (
	"testing"

	"github.com/sivchari/fjord/internal/eksd"
	"github.com/sivchari/fjord/internal/nodeimage"
)

func TestTag(t *testing.T) {
	t.Parallel()

	release := &eksd.Release{
		EKSVersion:  "1.33",
		Channel:     "1-33",
		Number:      29,
		KubeVersion: "v1.33.13",
	}

	want := "ghcr.io/sivchari/fjord/node:v1.33.13-eks-1-33-29"
	if got := nodeimage.Tag(release); got != want {
		t.Errorf("Tag() = %q, want %q", got, want)
	}
}

func TestFloatingTag(t *testing.T) {
	t.Parallel()

	release := &eksd.Release{
		EKSVersion:  "1.33",
		Channel:     "1-33",
		Number:      29,
		KubeVersion: "v1.33.13",
	}

	want := "ghcr.io/sivchari/fjord/node:1.33"
	if got := nodeimage.FloatingTag(release); got != want {
		t.Errorf("FloatingTag() = %q, want %q", got, want)
	}
}
