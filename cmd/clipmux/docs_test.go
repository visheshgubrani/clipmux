package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The site's command-line reference names every command and flag the binary
// has, so a new flag can't ship undocumented.
func TestTheCLIReferenceIsComplete(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "site", "content", "docs", "reference", "cli.mdx"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(page)

	for _, line := range strings.Split(usage, "\n") {
		// The commands are the indented lines.
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "clipmux" && !strings.Contains(doc, "clipmux "+f[1]) {
			t.Errorf("command %q is not on cli.mdx", "clipmux "+f[1])
		}
	}
	for _, line := range strings.Split(adminUsage, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "clipmux" && f[1] == "admin" &&
			!strings.Contains(doc, "clipmux admin "+f[2]) {
			t.Errorf("command %q is not on cli.mdx", "clipmux admin "+f[2])
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	flag := regexp.MustCompile(`fs\.(?:String|Bool|Duration|Int|Var)\("([a-z-]+)"`)
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range flag.FindAllStringSubmatch(string(src), -1) {
			seen++
			if !strings.Contains(doc, "--"+m[1]) {
				t.Errorf("flag --%s (%s) is not on cli.mdx", m[1], f)
			}
		}
	}
	if seen < 20 {
		t.Fatalf("found only %d flags; is the pattern wrong?", seen)
	}
}
