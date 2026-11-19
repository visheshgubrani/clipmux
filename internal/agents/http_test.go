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

// The Workers page's Docker command (P6-T07) runs the image of the server's
// own release, so its worker API matches; a CI build runs the sha- image of
// its commit, which CI pushed, and any other build the latest release.
func TestDockerCommandPinsTheServersImage(t *testing.T) {
	const run = `docker run -d --name clipmux-agent --restart unless-stopped --no-healthcheck --user "$(id -u):$(id -g)" ` +
		`-v "$HOME/.config/clipmux:/var/lib/clipmux-agent" -e CLIPMUX_AGENT_DIR=/var/lib/clipmux-agent `
	const tail = " worker --server https://video.example.com --pair ABCD2345"
	for _, c := range []struct{ version, image string }{
		{"v1.2.3", "ghcr.io/visheshgubrani/clipmux:v1.2.3"},
		{"v1.3.0-rc.2", "ghcr.io/visheshgubrani/clipmux:v1.3.0-rc.2"},
		{"0123456789abcdef0123456789abcdef01234567", "ghcr.io/visheshgubrani/clipmux:sha-0123456789abcdef0123456789abcdef01234567"},
		{"dev", "ghcr.io/visheshgubrani/clipmux:latest"},
	} {
		if got, want := dockerCommand(c.version, "https://video.example.com", "ABCD2345"), run+c.image+tail; got != want {
			t.Errorf("dockerCommand(%q) =\n  %s\nwant\n  %s", c.version, got, want)
		}
	}
}
