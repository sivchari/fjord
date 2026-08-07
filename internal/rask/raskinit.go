package rask

import (
	"embed"
	"errors"
	"io/fs"
)

// embedded holds the rask-init binary when one was produced at build time.
//
// It embeds the directory rather than the file so that a checkout without
// the binary still compiles: go:embed refuses to build on a missing file,
// but is content with a directory that does not contain it. That is what
// keeps the 47 MB build artifact out of git -- a checked-in placeholder
// would have to be tracked, and every build would overwrite it in place,
// one `git commit -a` away from shipping it.
//
//go:embed embedded
var embedded embed.FS

// raskInitPath is where the Makefile's rask-init target writes, and so
// where embedded carries the binary when it was built.
const raskInitPath = "embedded/rask-init"

// errNoRaskInit reports a macOS build that never ran the rask-init step.
//
// Not returning it would be worse than it looks: rask treats nil bytes as
// "use my own copy", and rask cannot ship one inside its module either, so
// the VM would boot a placeholder and fail minutes later as an unexplained
// timeout.
var errNoRaskInit = errors.New("this fjord binary was built without rask-init, the PID 1 of the VM a macOS cluster runs in: run `make rask-init` and build again, or use a released binary")

// resolveRaskInit returns the linux/arm64 rask-init binary to hand rask on
// goos, reading it from fsys.
//
// Only macOS needs it. On Linux rask runs the control plane as host
// processes with no VM, so its absence is not merely tolerable there but
// expected: nothing in a Linux build ever produces it.
func resolveRaskInit(fsys fs.FS, goos string) ([]byte, error) {
	data, err := fs.ReadFile(fsys, raskInitPath)
	if err != nil {
		if goos == "darwin" {
			return nil, errNoRaskInit
		}

		return nil, nil
	}

	return data, nil
}
