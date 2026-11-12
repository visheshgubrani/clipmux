package pairing

import (
	"regexp"
	"testing"
)

// A code is typed back from a screen: any case, with or without the dash or
// spaces, and never with a look-alike character.
func TestCodesTypeBackAsShown(t *testing.T) {
	for in, want := range map[string]string{
		"KXFT-9M2Q":   "KXFT9M2Q",
		"kxft9m2q":    "KXFT9M2Q",
		" kxft 9m2q ": "KXFT9M2Q",
		"KXFT-9M2":    "",
		"KXFT-9M2QQ":  "",
		"KXF0-9M2Q":   "", // 0 is not in the alphabet
		"KXFI-9M2Q":   "", // nor I
		"":            "",
	} {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	shape := regexp.MustCompile(`^[A-HJKMNP-Z2-9]{4}-[A-HJKMNP-Z2-9]{4}$`)
	for range 100 {
		c := NewCode()
		if !shape.MatchString(c) || NormalizeCode(c) == "" {
			t.Fatalf("NewCode() = %q", c)
		}
	}
}
