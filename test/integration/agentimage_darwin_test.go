//go:build integration && darwin

package integration

// agentImageArgs pulls the published agent image rather than building one.
//
// Building it needs a Linux Docker daemon, which on macOS means a VM of its
// own -- running alongside the Virtualization.framework VM the cluster
// itself lives in, on a laptop. What these tests exist to prove here is that
// the vz substrate carries fjord's EKS emulation, not that the agent's
// Dockerfile builds; Linux CI already covers the latter on every pull
// request.
func agentImageArgs() []string { return nil }
