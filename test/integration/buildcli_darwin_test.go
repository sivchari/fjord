//go:build integration && darwin

package integration

import (
	"os/exec"
	"testing"
)

// prepareCLI makes a freshly built fjord usable on macOS, where a plain `go
// build` produces a binary that cannot start a cluster at all:
//
//   - The VM's PID 1 (rask-init) is only a placeholder in a clean checkout,
//     and rask rejects it when the provider is constructed.
//   - Virtualization.framework refuses to start a VM for a binary without
//     the com.apple.security.virtualization entitlement.
//
// Both are build-time facts that the release pipeline handles and a test
// binary otherwise misses, which is why these tests never ran on macOS.
func prepareCLI(t *testing.T, bin string) {
	t.Helper()

	raskInit := exec.Command("make", "rask-init")
	raskInit.Dir = "../.."

	if out, err := raskInit.CombinedOutput(); err != nil {
		t.Fatalf("make rask-init: %v\n%s", err, out)
	}

	// Rebuild so the binary embeds the rask-init just produced; the caller
	// built before this ran.
	rebuild := exec.Command("go", "build", "-o", bin, "./cmd/fjord")
	rebuild.Dir = "../.."

	if out, err := rebuild.CombinedOutput(); err != nil {
		t.Fatalf("rebuild fjord with rask-init: %v\n%s", err, out)
	}

	sign := exec.Command("codesign", "--entitlements", "vz.entitlements", "-f", "-s", "-", bin)
	sign.Dir = "../.."

	if out, err := sign.CombinedOutput(); err != nil {
		t.Fatalf("codesign fjord: %v\n%s", err, out)
	}
}
