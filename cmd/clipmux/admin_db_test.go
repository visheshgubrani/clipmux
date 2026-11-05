package main

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

var keyLine = regexp.MustCompile(`key:\s+(cmx_\S+)`)

func TestCreateKeyCreatesTheAppOnceAndPrintsAWorkingKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)

	var first, second bytes.Buffer
	if err := createKey(ctx, pool, "LMS", "backend", []string{apikeys.UploadsWrite}, &first); err != nil {
		t.Fatal(err)
	}
	if err := createKey(ctx, pool, "LMS", "rotation", []string{apikeys.AssetsRead}, &second); err != nil {
		t.Fatal(err)
	}
	var apps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE name = 'LMS'`).Scan(&apps); err != nil || apps != 1 {
		t.Fatalf("apps named LMS = %d, %v; want 1", apps, err)
	}
	m := keyLine.FindStringSubmatch(first.String())
	if m == nil {
		t.Fatalf("no key in output:\n%s", first.String())
	}
	key, err := apikeys.Authenticate(ctx, pool, m[1])
	if err != nil || !key.Can(apikeys.UploadsWrite) {
		t.Fatalf("printed key: %+v, %v", key, err)
	}
}

func TestAdminRequiresAnApp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := admin([]string{"create-key"}, func(string) string { return "" }, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if code := admin(nil, func(string) string { return "" }, &out, &errOut); code != 2 {
		t.Fatalf("no subcommand: exit = %d, want 2", code)
	}
}
