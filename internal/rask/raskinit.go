package rask

import _ "embed"

// raskInit is the linux/arm64 rask-init binary rask boots as PID 1 inside
// the macOS VM it runs a cluster in. rask cannot ship it inside its own Go
// module (a committed binary), so a consumer supplies it instead, via
// raskcluster.WithRaskInit.
//
// It is produced at build time, not at run time, because that is the only
// moment a Go toolchain is guaranteed to be present:
//
//	GOOS=linux GOARCH=arm64 go build -o internal/rask/embedded/rask-init \
//	    github.com/sivchari/rask/cmd/rask-init
//
// rask is already a module dependency, so that resolves from the module
// cache with no network access. See the darwin build's pre hook in
// .goreleaser.yml and the `rask-init` target in the Makefile.
//
// The checked-in file is a placeholder: linux clusters never read it (rask
// runs the control plane as host processes there), and go:embed refuses to
// build without something to embed. Booting a macOS VM with the placeholder
// fails at provider construction, in microseconds, with rask naming the
// build step to run -- not minutes later as an unexplained VM timeout.
//
//go:embed embedded/rask-init
var raskInit []byte
