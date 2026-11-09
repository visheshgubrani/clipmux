package agents

import "testing"

// The Workers page's Linux one-liner (P6-T08) fetches install-agent.sh from
// the server's own release, so the agent it installs matches the server; a
// build that isn't a release takes the latest.
func TestInstallCommandNamesTheServersRelease(t *testing.T) {
	for _, c := range []struct{ version, want string }{
		{"v1.2.3", "curl -fsSL https://github.com/visheshgubrani/clipmux/releases/download/v1.2.3/install-agent.sh | sh -s -- --server https://video.example.com --pair ABCD2345"},
		{"v1.3.0-rc.2", "curl -fsSL https://github.com/visheshgubrani/clipmux/releases/download/v1.3.0-rc.2/install-agent.sh | sh -s -- --server https://video.example.com --pair ABCD2345"},
		{"dev", "curl -fsSL https://github.com/visheshgubrani/clipmux/releases/latest/download/install-agent.sh | sh -s -- --server https://video.example.com --pair ABCD2345"},
		{"0123456789abcdef0123456789abcdef01234567", "curl -fsSL https://github.com/visheshgubrani/clipmux/releases/latest/download/install-agent.sh | sh -s -- --server https://video.example.com --pair ABCD2345"},
	} {
		if got := installCommand(c.version, "https://video.example.com", "ABCD2345"); got != c.want {
			t.Errorf("installCommand(%q) =\n  %s\nwant\n  %s", c.version, got, c.want)
		}
	}
}
