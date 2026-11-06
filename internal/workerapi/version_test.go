package workerapi

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v3.9.3", [3]int{3, 9, 3}, true},
		{"3.9.1", [3]int{3, 9, 1}, true},
		{"v3.9.3-0a8ba4f-release", [3]int{3, 9, 3}, true}, // what `packager --version` prints
		{"v3.10.0", [3]int{3, 10, 0}, true},
		{"", [3]int{}, false},
		{"v3.9", [3]int{}, false},
		{"latest", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := ParseVersion(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if CompareVersion([3]int{3, 10, 0}, MinPackager) <= 0 {
		t.Error("3.10.0 must be newer than 3.9.1 (numeric, not lexical)")
	}
	if CompareVersion([3]int{3, 9, 0}, MinPackager) >= 0 {
		t.Error("3.9.0 must be older than 3.9.1")
	}
}
