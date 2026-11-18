package manage_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The site's management API page lists exactly the owner routes serve
// registers: every route in this package, the setup and session routes in
// internal/accounts, and the owner's routes in internal/agents and
// internal/apppairings (the ones wrapped in owned). A new route can't ship
// undocumented, and the page can't list one that's gone.
func TestTheManagementReferenceIsComplete(t *testing.T) {
	registered := map[string]string{}
	pattern := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ /[^"]*)"`)
	add := func(glob string, ownedOnly bool) {
		t.Helper()
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(src), "\n") {
				if ownedOnly && !strings.Contains(line, ".owned(") {
					continue
				}
				if m := pattern.FindStringSubmatch(line); m != nil {
					registered[m[1]] = f
				}
			}
		}
	}
	add("*.go", false)
	add(filepath.Join("..", "accounts", "*.go"), false)
	add(filepath.Join("..", "agents", "*.go"), true)
	add(filepath.Join("..", "apppairings", "*.go"), true)
	if len(registered) < 80 {
		t.Fatalf("found only %d owner routes; is the pattern wrong?", len(registered))
	}

	page, err := os.ReadFile(filepath.Join("..", "..", "site", "content", "docs", "reference", "management-api.mdx"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("`((?:GET|POST|PUT|PATCH|DELETE) /[^`\\s]*)`").FindAllStringSubmatch(string(page), -1) {
		documented[m[1]] = true
	}

	for _, r := range sortedKeys(registered) {
		if !documented[r] {
			t.Errorf("%s (%s) is not on management-api.mdx", r, registered[r])
		}
	}
	for _, r := range sortedKeys(documented) {
		if _, ok := registered[r]; !ok {
			t.Errorf("management-api.mdx lists %s, which serve doesn't register as an owner route", r)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
