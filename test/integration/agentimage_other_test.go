//go:build integration && !darwin

package integration

// agentImageArgs builds the agent image from the working tree, so a change
// to the agent is exercised by the same run that changed it.
func agentImageArgs() []string { return []string{"--build-local"} }
