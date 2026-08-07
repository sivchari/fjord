//go:build integration && !darwin

package integration

import "testing"

// prepareCLI is a no-op off macOS: a plain `go build` already produces a
// binary that can create a cluster. See the darwin implementation for what
// that platform additionally needs.
func prepareCLI(_ *testing.T, _ string) {}
