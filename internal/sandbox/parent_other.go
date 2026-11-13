//go:build !linux

package sandbox

import "os/exec"

// DieWithParent does nothing: only Linux has a parent-death signal.
func DieWithParent(*exec.Cmd) {}

// Undumpable does nothing: only Linux exposes a process's environment to
// other processes of its user.
func Undumpable() error { return nil }
