package ids

import (
	"strings"
	"testing"
	"time"
)

func TestNewHasPrefixAndLength(t *testing.T) {
	id := New(Asset)
	if !strings.HasPrefix(id, "ast_") || len(id) != len("ast_")+26 {
		t.Fatalf("id = %q", id)
	}
}

func TestIDsSortByCreationTime(t *testing.T) {
	a := newAt(Job, time.UnixMilli(1_700_000_000_000))
	b := newAt(Job, time.UnixMilli(1_700_000_000_001))
	if !(a < b) {
		t.Fatalf("%q should sort before %q", a, b)
	}
}

func TestEncodeWorkedExample(t *testing.T) {
	// 128 bits of zero then a single 1 in the lowest bit.
	var b [16]byte
	b[15] = 1
	if got := encode(b); got != "00000000000000000000000001" {
		t.Fatalf("encode = %q", got)
	}
	// Top 3 bits set: 128 = 26*5 - 2, so the first character holds 3 bits.
	b = [16]byte{0xE0}
	if got := encode(b); got != "70000000000000000000000000" {
		t.Fatalf("encode = %q", got)
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id := New(Event)
		if seen[id] {
			t.Fatalf("duplicate %q", id)
		}
		seen[id] = true
	}
}
