//go:build integration

package integration

import (
	"bufio"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// agentURL is the base URL subtests use to reach fjord-agent's fake AWS
// APIs, published by startAgentPortForward before any subtest runs.
var agentURL string

// startAgentPortForward opens a tunnel to fjord-agent with `fjord
// port-forward` and records the address it prints, keeping the tunnel up
// until the test that called it finishes.
//
// The tests do not dial the cluster's NodePort directly. That only works
// where the host is the node -- true of rask's hostproc runtime on Linux,
// false on macOS, where the cluster runs inside a VM and the NodePort is not
// on this machine's loopback. Going through the same command a user would
// run keeps these tests honest about what actually works on each platform.
func startAgentPortForward(t *testing.T, bin string) {
	t.Helper()

	cmd := exec.Command(bin, "port-forward", "--name", clusterName)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("port-forward stdout: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start port-forward: %v", err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		agentURL = ""
	})

	// The command prints the bound URL on its first line and then blocks,
	// so one line read doubles as the handshake that the tunnel is up.
	lines := make(chan string, 1)

	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr != nil {
			// Report the empty line: the timeout below turns it into
			// a failure, and t.Fatalf off the test goroutine would not.
			line = ""
		}

		lines <- line
	}()

	select {
	case line := <-lines:
		agentURL = strings.TrimSpace(line)
		if agentURL == "" {
			t.Fatal("port-forward exited without reporting an address")
		}
	case <-time.After(portForwardTimeout):
		t.Fatalf("port-forward did not report an address within %s", portForwardTimeout)
	}
}

// agentURLFor returns the tunnel's base URL, failing the subtest rather than
// silently sending an AWS CLI call to an empty endpoint.
func agentURLFor(t *testing.T) string {
	t.Helper()

	if agentURL == "" {
		t.Fatal("fjord-agent port-forward is not running")
	}

	return agentURL
}

// portForwardTimeout bounds how long the tunnel may take to report its
// address. It is generous because on macOS the request crosses into the VM.
const portForwardTimeout = 30 * time.Second
