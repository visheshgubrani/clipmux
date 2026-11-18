package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every variable a deployment reads is documented on the site's
// configuration page and has an example in .env.example; a variable only
// tests read is documented there or in docs/configuration.md.
func TestEveryVariableIsDocumented(t *testing.T) {
	root := filepath.Join("..", "..")
	public, err := os.ReadFile(filepath.Join(root, "site", "content", "docs", "reference", "configuration.mdx"))
	if err != nil {
		t.Fatal(err)
	}
	dev, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`"(CLIPMUX_[A-Z0-9_]+|DATABASE_URL)"`)
	found := map[string]bool{} // name: read outside tests
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// Test files, test helper packages and the exit tests don't
			// configure a deployment.
			testOnly := strings.HasSuffix(p, "_test.go") || strings.Contains(p, "test"+string(filepath.Separator)) ||
				strings.Contains(p, filepath.Join("internal", "e2e"))
			for _, m := range name.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = found[m[1]] || !testOnly
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) < 30 {
		t.Fatalf("found only %d variables; is the walk wrong?", len(found))
	}
	for v, deployed := range found {
		in := func(doc []byte) bool {
			return strings.Contains(string(doc), "`"+v+"`") ||
				(strings.HasPrefix(v, "CLIPMUX_E2E_") && strings.Contains(string(doc), "`CLIPMUX_E2E_*`"))
		}
		switch {
		case deployed && !in(public):
			t.Errorf("%s is read but not on site/content/docs/reference/configuration.mdx", v)
		case !deployed && !in(public) && !in(dev):
			t.Errorf("%s is read by tests but in neither configuration page", v)
		}
		if deployed && !strings.Contains(string(example), v+"=") {
			t.Errorf("%s is read but has no example in .env.example", v)
		}
	}
}
