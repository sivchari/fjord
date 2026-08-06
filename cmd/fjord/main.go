// Command fjord creates local Kubernetes clusters that behave like Amazon
// EKS from the inside.
package main

import (
	"fmt"
	"os"

	raskcluster "github.com/sivchari/rask/pkg/cluster"

	"github.com/sivchari/fjord/cli"
)

// version is injected by goreleaser via -ldflags "-X main.version=...".
var version string

func main() {
	// rask hosts each macOS VM in a detached re-exec of this very binary, so
	// that the cluster outlives the command that created it. That re-exec is
	// recognised by argv[1] alone, before any flag parsing, so this has to
	// run ahead of cobra. On Linux it is a no-op.
	if handled, err := raskcluster.RunVMHostIfRequested(); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}

		return
	}

	cmd := cli.NewRootCmd()
	if version != "" {
		cmd.Version = version
	}

	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln("Error:", err.Error())
		os.Exit(1)
	}
}
