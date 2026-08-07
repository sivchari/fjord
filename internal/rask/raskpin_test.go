package rask

import (
	"os"
	"regexp"
	"testing"
)

// raskRequire matches the rask requirement line in a go.mod.
var raskRequire = regexp.MustCompile(`(?m)^\s*github\.com/sivchari/rask (v\S+)`)

// TestRaskPinsMatch fails when fjord's two rask pins have drifted apart.
//
// fjord depends on rask twice: go.mod for the pkg/cluster API it drives the
// cluster through, and raskinit/go.mod for cmd/rask-init, the binary rask
// boots as PID 1 inside the VM a macOS cluster runs in. Nothing else ties
// them together, and only the first is visible in day-to-day work.
//
// That gap cost real time: raskinit/go.mod sat on v0.1.9 across six bumps of
// the other, so a fix that shipped in rask-init never reached any fjord
// build, and macOS kept failing in a way that looked like rask had not fixed
// it. Upgrading one without the other is always a mistake.
func TestRaskPinsMatch(t *testing.T) {
	t.Parallel()

	// Literal paths, read inline: routing them through a helper only hides
	// from the reader that both are fixed.
	apiMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	pidOneMod, err := os.ReadFile("../../raskinit/go.mod")
	if err != nil {
		t.Fatalf("read raskinit/go.mod: %v", err)
	}

	api := raskVersion(t, "go.mod", apiMod)
	pidOne := raskVersion(t, "raskinit/go.mod", pidOneMod)

	if api != pidOne {
		t.Errorf("rask is pinned to %s in go.mod but %s in raskinit/go.mod;\n"+
			"run: go get github.com/sivchari/rask@%s && go get -modfile raskinit/go.mod github.com/sivchari/rask@%s",
			api, pidOne, api, api)
	}
}

// raskVersion returns the rask version required by the go.mod named name.
func raskVersion(t *testing.T, name string, mod []byte) string {
	t.Helper()

	match := raskRequire.FindSubmatch(mod)
	if match == nil {
		t.Fatalf("%s does not require github.com/sivchari/rask", name)
	}

	return string(match[1])
}
