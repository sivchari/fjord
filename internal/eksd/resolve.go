package eksd

import (
	"fmt"
	"sort"
	"strings"
)

// UnsupportedVersionError reports that Resolve was called with an EKS
// Kubernetes minor version absent from the generated release table.
type UnsupportedVersionError struct {
	EKSVersion string
	Supported  []string
}

// Error implements the error interface.
func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf("unsupported eks version %q, supported versions: %s", e.EKSVersion, strings.Join(e.Supported, ", "))
}

// Resolve returns the EKS-D release information for eksVersion (e.g.
// "1.33"), looked up from the table internal/cmd/eksd-gen generates.
// Resolve performs no network I/O.
func Resolve(eksVersion string) (*Release, error) {
	release, ok := table[eksVersion]
	if !ok {
		return nil, &UnsupportedVersionError{EKSVersion: eksVersion, Supported: SupportedVersions()}
	}

	return &release, nil
}

// SupportedVersions returns the sorted list of EKS Kubernetes minor
// versions available in the generated release table.
func SupportedVersions() []string {
	versions := make([]string, 0, len(table))
	for v := range table {
		versions = append(versions, v)
	}

	sort.Strings(versions)

	return versions
}
