// Command fjord creates local Kubernetes clusters that behave like Amazon
// EKS from the inside.
package main

import (
	"os"

	"github.com/sivchari/fjord/cli"
)

// version is injected by goreleaser via -ldflags "-X main.version=...".
var version string

func main() {
	cmd := cli.NewRootCmd()
	if version != "" {
		cmd.Version = version
	}

	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln("Error:", err.Error())
		os.Exit(1)
	}
}
