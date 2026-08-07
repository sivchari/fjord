# `internal/rask/embedded`

This directory exists to hold `rask-init`, the linux/arm64 binary rask boots
as PID 1 inside the VM a macOS cluster runs in. It is produced during fjord's
own build, by the `rask-init` target in the Makefile and by the darwin
build's `pre` hook in `.goreleaser.yml`.

The binary itself is deliberately **not** checked in, and `.gitignore` keeps
it that way. rask cannot ship it inside its own Go module, so fjord embeds it
-- but at ~47 MB it has no business in git history, and a tracked copy that
every build overwrites in place is one `git commit -a` away from being
committed by accident.

`internal/rask/raskinit.go` embeds this directory rather than the file, so a
checkout without the binary still compiles. On Linux that is the normal
state: rask runs the control plane as host processes, with no VM and no
PID 1 to supply.
