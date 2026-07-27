//go:build integration

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// agentClusterName is the cluster name used by the fjord-agent e2e test, kept
// distinct from TestCreateCluster's clusterName so the two tests never
// collide if run concurrently.
const agentClusterName = clusterName + "-agent"

// agentHostPort is the host port fjord-agent's fake STS API is published on
// for this test.
const agentHostPort = "48080"

// TestAgentSTSGetCallerIdentity is e2e① from plan-v1.md Phase 3: it creates
// a cluster with fjord-agent auth enabled, registers a principal, and
// verifies `aws sts get-caller-identity --endpoint-url` against
// fjord-agent's fake STS NodePort resolves the registered principal's ARN
// from its access key credentials.
//
// This builds a full cluster (~tens of minutes) and requires the `aws` CLI,
// so it is not run as part of the default suite. Run it explicitly:
//
//	go test -C test -tags=integration -run TestAgentSTSGetCallerIdentity -v ./integration/...
func TestAgentSTSGetCallerIdentity(t *testing.T) {
	t.Skip("run explicitly: builds a full cluster with fjord-agent auth enabled")

	bin := buildCLI(t)

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "cluster", "--name", agentClusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete cluster: %v\n%s", err, out)
		}
	})

	create := exec.Command(bin, "create", "cluster",
		"--eks-version", eksVersion,
		"--name", agentClusterName,
		"--build-local",
		"--agent-host-port", agentHostPort,
	)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr

	if err := create.Run(); err != nil {
		t.Fatalf("create cluster: %v", err)
	}

	principal := exec.Command(bin, "create", "principal", "alice", "--name", agentClusterName)

	principalOut, err := principal.CombinedOutput()
	if err != nil {
		t.Fatalf("create principal: %v\n%s", err, principalOut)
	}

	accessKeyID, wantArn := parsePrincipalOutput(t, string(principalOut))

	assertCallerIdentity(t, accessKeyID, wantArn)
}

// parsePrincipalOutput extracts the access-key-id and arn fields from
// `fjord create principal`'s stdout.
func parsePrincipalOutput(t *testing.T, output string) (accessKeyID, arn string) {
	t.Helper()

	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "access-key-id: "):
			accessKeyID = strings.TrimPrefix(line, "access-key-id: ")
		case strings.HasPrefix(line, "arn: "):
			arn = strings.TrimPrefix(line, "arn: ")
		}
	}

	if accessKeyID == "" || arn == "" {
		t.Fatalf("could not parse principal output: %q", output)
	}

	return accessKeyID, arn
}

// assertCallerIdentity runs `aws sts get-caller-identity` against
// fjord-agent's fake STS endpoint (published on agentHostPort by its
// NodePort Service) using accessKeyID as the SigV4 credential, and verifies
// it resolves to wantArn.
func assertCallerIdentity(t *testing.T, accessKeyID, wantArn string) {
	t.Helper()

	cmd := exec.Command("aws", "sts", "get-caller-identity",
		"--endpoint-url", "http://localhost:"+agentHostPort,
	)
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+accessKeyID,
		"AWS_SECRET_ACCESS_KEY=fake",
		"AWS_REGION=us-east-1",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aws sts get-caller-identity: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), wantArn) {
		t.Errorf("get-caller-identity output = %s, want it to contain %q", out, wantArn)
	}
}
