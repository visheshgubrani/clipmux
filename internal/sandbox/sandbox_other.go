//go:build !linux

package sandbox

// apply refuses: only Linux confines the tools.
func apply(Policy, []string) (Status, error) { return Status{}, ErrUnsupported }
