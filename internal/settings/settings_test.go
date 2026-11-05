package settings

import "testing"

// No API key can be created while the public URL is loopback: presigned URLs
// and base_url would point at the owner's own machine (docs/architecture.md,
// "First boot", step 3). LAN addresses are real deployments.
func TestKeysAllowedOnlyOffLoopback(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8080":         false,
		"http://127.0.0.1:8080":         false,
		"http://127.1.2.3":              false,
		"http://clipmux.localhost":      false,
		"http://[::1]:8080":             false,
		"http://0.0.0.0:8080":           false,
		"":                              false,
		"http://192.168.1.10:8080":      true,
		"https://video.example.com":     true,
		"https://localhost.example.com": true,
	}
	for url, want := range cases {
		if got := KeysAllowed(url); got != want {
			t.Errorf("KeysAllowed(%q) = %v, want %v", url, got, want)
		}
	}
}
