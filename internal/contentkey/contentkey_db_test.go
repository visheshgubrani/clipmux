package contentkey

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

func newBox(t *testing.T) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(1, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sealedSecrets(t *testing.T, pool *pgxpool.Pool) [][]byte {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT secret_encrypted FROM content_key_secrets ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

func TestFirstBootStoresOnlyCiphertextAndDerivesTheSameKeyAfterReload(t *testing.T) {
	ctx := context.Background()
	pool, box, dir := dbtest.New(t), newBox(t), t.TempDir()
	a, err := Load(ctx, pool, box, "prj_default", dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Current().Version != 1 {
		t.Fatalf("first secret version = %d, want 1", a.Current().Version)
	}
	sealed := sealedSecrets(t, pool)
	if len(sealed) != 1 || bytes.Contains(sealed[0], a.Current().key) || strings.Contains(string(sealed[0]), hex.EncodeToString(a.Current().key)) {
		t.Fatal("the database must hold exactly one sealed secret, never the secret itself")
	}
	if _, err := os.Stat(filepath.Join(dir, "content.key")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("no content.key file is written any more")
	}
	b, err := Load(ctx, pool, box, "prj_default", dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Current().Derive("ast_1", "out_1") != b.Current().Derive("ast_1", "out_1") {
		t.Fatal("a reload must derive the same keys")
	}
	other, _ := secrets.NewBox(1, bytes.Repeat([]byte{4}, 32))
	if _, err := Load(ctx, pool, other, "prj_default", dir); err == nil {
		t.Fatal("a secrets key that can't open the stored secret must fail the boot")
	}
}

// Installs from before P1-T10 keep their secret in {data_dir}/content.key.
// The vector is TestDerivationVectors'.
func TestContentKeyFileIsImportedOnceAndRemoved(t *testing.T) {
	ctx := context.Background()
	pool, box, dir := dbtest.New(t), newBox(t), t.TempDir()
	file := filepath.Join(dir, "content.key")
	secret := "v1:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if err := os.WriteFile(file, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := Load(ctx, pool, box, "prj_default", dir)
	if err != nil {
		t.Fatal(err)
	}
	key := ring.Current().Derive("ast_1", "out_1")
	if got := hex.EncodeToString(key[:]); got != "8b60ee274c0f0c75195f891d401f5c7f" {
		t.Fatalf("imported secret derives %s", got)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("content.key must be removed once the database holds it")
	}
	again, err := Load(ctx, pool, box, "prj_default", dir)
	if err != nil || again.Current().Derive("ast_1", "out_1") != key {
		t.Fatalf("reload after import: %v", err)
	}
	// A file that disagrees with the database is a restore gone wrong.
	if err := os.WriteFile(file, []byte("v1:"+strings.Repeat("ff", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ctx, pool, box, "prj_default", dir); err == nil {
		t.Fatal("a content.key that disagrees with the database must fail the boot")
	}
}

func TestBootRefusesWhenAnOutputsKeyVersionIsMissing(t *testing.T) {
	ctx := context.Background()
	pool, box := dbtest.New(t), newBox(t)
	if _, err := Load(ctx, pool, box, "prj_default", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
		INSERT INTO outputs (id, project_id, asset_id, key_version) VALUES ('out_1', 'prj_default', 'ast_1', 3);`); err != nil {
		t.Fatal(err)
	}
	_, err := Load(ctx, pool, box, "prj_default", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "version 3") {
		t.Fatalf("Load = %v, want a refusal naming version 3", err)
	}
}

func TestARotatedVersionIsUsedOnlyOnceUsableAndEveryCopySeesIt(t *testing.T) {
	ctx := context.Background()
	pool, box, dir := dbtest.New(t), newBox(t), t.TempDir()
	ring, err := Load(ctx, pool, box, "prj_default", dir)
	if err != nil {
		t.Fatal(err)
	}
	held := ring // what the worker API, delivery and Connect hold
	v, err := AddVersion(ctx, pool, box, "prj_default")
	if err != nil || v != 2 {
		t.Fatalf("AddVersion = %d, %v; want 2", v, err)
	}
	reloaded, err := Load(ctx, pool, box, "prj_default", dir)
	if err != nil {
		t.Fatal(err)
	}
	ring.Replace(reloaded)
	if held.Current().Version != 1 {
		t.Fatalf("a pending version is current: %d", held.Current().Version)
	}
	if _, ok := held.Get(2); !ok || !strings.Contains(held.WorkerSecret(), `"2":`) {
		t.Fatal("a pending version must be in the ring, so the Worker can be given it")
	}
	if err := MakeUsable(ctx, pool, "prj_default"); err != nil {
		t.Fatal(err)
	}
	reloaded, err = Load(ctx, pool, box, "prj_default", dir)
	if err != nil {
		t.Fatal(err)
	}
	ring.Replace(reloaded)
	if held.Current().Version != 2 {
		t.Fatalf("after MakeUsable current = %d, want 2 in every copy", held.Current().Version)
	}
	if one, _ := held.Get(1); one.Derive("ast_1", "out_1") == held.Current().Derive("ast_1", "out_1") {
		t.Fatal("a new version must derive different keys")
	}
}
