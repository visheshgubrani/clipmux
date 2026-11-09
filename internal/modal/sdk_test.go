package modal

import (
	"os"
	"path/filepath"
	"testing"
)

// serve may run with no HOME (a systemd unit, the exit tests' rig), and it
// must never read the user's ~/.modal.toml: the token from Connect is all
// it uses. The real-provider run found the SDK refusing without a home.
func TestTheClientNeedsNoHomeAndReadsNoProfile(t *testing.T) {
	t.Setenv("MODAL_CONFIG_PATH", "")
	t.Setenv("HOME", "")
	if c, err := newSDK("ak-test", "as-test", ""); err != nil {
		t.Fatalf("with no HOME: %v", err)
	} else {
		c.close()
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".modal.toml"), []byte("not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := newSDK("ak-test", "as-test", ""); err != nil {
		t.Fatalf("a broken ~/.modal.toml was read: %v", err)
	} else {
		c.close()
	}
}
