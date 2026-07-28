//go:build integration

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// agentPrincipalName is the principal fjord create principal registers for
// verifyAgentSTSGetCallerIdentity, kept distinct from every other subtest's
// principal names so none of them collide.
const agentPrincipalName = "agent-e2e-alice"

// verifyAgentSTSGetCallerIdentity is e2e① from plan-v1.md Phase 3: it
// registers a principal via `fjord create principal` and verifies that `aws
// sts get-caller-identity --endpoint-url` against fjord-agent's fake STS
// NodePort resolves the registered principal's ARN from its access key
// credentials.
func verifyAgentSTSGetCallerIdentity(t *testing.T, bin string) {
	t.Helper()

	t.Cleanup(func() {
		cmd := exec.Command(bin, "delete", "principal", agentPrincipalName, "--name", clusterName)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("delete principal %q: %v\n%s", agentPrincipalName, err, out)
		}
	})

	principal := exec.Command(bin, "create", "principal", agentPrincipalName, "--name", clusterName)

	principalOut, err := principal.CombinedOutput()
	if err != nil {
		t.Fatalf("create principal: %v\n%s", err, principalOut)
	}

	accessKeyID, wantARN := parsePrincipalOutput(t, string(principalOut))

	assertCallerIdentity(t, accessKeyID, wantARN)
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
