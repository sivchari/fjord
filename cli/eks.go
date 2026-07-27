package cli

import (
	"strconv"
	"strings"
)

// latestVersion returns the highest EKS minor version in versions, comparing
// the numeric minor component rather than the lexical string.
func latestVersion(versions []string) string {
	latest := ""
	latestMinor := -1

	for _, v := range versions {
		_, minorStr, ok := strings.Cut(v, ".")
		if !ok {
			continue
		}

		minor, err := strconv.Atoi(minorStr)
		if err != nil {
			continue
		}

		if minor > latestMinor {
			latestMinor = minor
			latest = v
		}
	}

	return latest
}

// splitImageRef splits an image reference into repository and tag. The tag
// separator is the last colon that appears after the last slash, so registry
// ports are not mistaken for tags.
func splitImageRef(ref string) (repository, tag string) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 || strings.Contains(ref[idx:], "/") {
		return ref, ""
	}

	return ref[:idx], ref[idx+1:]
}
