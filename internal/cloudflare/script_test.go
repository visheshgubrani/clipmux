package cloudflare

import "testing"

func TestScriptForNamesTheWorkerAfterItsHostname(t *testing.T) {
	for host, want := range map[string]string{
		"media.clipmux.com":      "clipmux-media-clipmux-com",
		"media-test.clipmux.com": "clipmux-media-test-clipmux-com",
		"v.example.net":          "clipmux-v-example-net",
		// Cloudflare caps script names at 63 characters; a long hostname
		// keeps its start and a hash of the whole, so two still differ.
		"a-very-long-media-subdomain-name.for-a-tenant.example.co.uk": "clipmux-a-very-long-media-subdomain-name-for-a-tenant-6fdfe8c6",
	} {
		if got := ScriptFor(host); got != want {
			t.Errorf("ScriptFor(%q) = %q, want %q", host, got, want)
		}
		if n := len(ScriptFor(host)); n > 63 {
			t.Errorf("ScriptFor(%q) is %d characters", host, n)
		}
	}
}
