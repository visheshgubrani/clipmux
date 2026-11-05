package media

import (
	"os"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/sandbox"
)

// The test binary doubles as `clipmux sandbox-exec` for the sandboxed tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(sandbox.Main(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
