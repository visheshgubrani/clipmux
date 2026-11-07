//go:build !linux

package sandbox

import "os/exec"

// DieWithParent does nothing: only Linux has a parent-death signal.
func DieWithParent(*exec.Cmd) {}
