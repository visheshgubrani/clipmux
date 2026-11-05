package stores

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
)

func testBucket(t *testing.T) (S3Config, S3Secret) {
	t.Helper()
	b := s3test.New(t)
	return S3Config{Endpoint: b.Endpoint, Bucket: b.Name}, S3Secret{AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey}
}

func newSet(t *testing.T) (*Set, *pgxpool.Pool, *storage.Filesystem, *secrets.Box) {
	t.Helper()
	pool := dbtest.New(t)
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost:8080"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{4}, 32))
	return New(pool, box, fs), pool, fs, box
}

func TestAnS3ConfigIsCheckedThenStoredSealed(t *testing.T) {
	cfg, sec := testBucket(t)
	set, pool, fs, box := newSet(t)
	ctx := context.Background()
	row, err := set.AddS3(ctx, db.DefaultProjectID, KindS3, cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.SecretsEncrypted, []byte(sec.SecretAccessKey)) || bytes.Contains(row.Config, []byte(sec.SecretAccessKey)) {
		t.Fatal("the secret access key is stored in the clear")
	}
	// A fresh Set (a restarted serve) opens it from Postgres.
	again := New(pool, box, fs)
	st, err := again.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*storage.S3); !ok {
		t.Fatalf("Get = %T, want the S3 adapter", st)
	}
	if err := st.Put(ctx, "media/a/o/x", strings.NewReader("on s3")); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Get(ctx, "media/a/o/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "on s3" {
		t.Fatalf("read back %q", b)
	}
	// Under another secrets key the credentials don't open.
	other, _ := secrets.NewBox(1, bytes.Repeat([]byte{5}, 32))
	if _, err := New(pool, other, fs).Get(ctx, row.ID); err == nil {
		t.Fatal("credentials opened under the wrong secrets key")
	}
}

func TestBadCredentialsStoreNothing(t *testing.T) {
	cfg, sec := testBucket(t)
	set, pool, _, _ := newSet(t)
	sec.SecretAccessKey = "wrong-" + sec.SecretAccessKey
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := set.AddS3(ctx, db.DefaultProjectID, KindS3, cfg, sec); err == nil {
		t.Fatal("AddS3 accepted credentials the store refuses")
	}
	rows, err := db.New(pool).ListStorageConfigs(ctx, db.DefaultProjectID)
	if err != nil || len(rows) != 1 || rows[0].Kind != KindFilesystem {
		t.Fatalf("configs = %+v, %v; want only the filesystem default", rows, err)
	}
}

// Changing the default moves new uploads only; old assets keep playing
// from where they are (docs/architecture.md, "Per-asset storage").
func TestEachAssetKeepsItsOwnStore(t *testing.T) {
	cfg, sec := testBucket(t)
	set, pool, fs, _ := newSet(t)
	ctx := context.Background()
	q := db.New(pool)
	app, err := q.GetBuiltinApp(ctx, db.DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	fsID, _, err := set.Default(ctx, db.DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := q.CreateAsset(ctx, db.CreateAssetParams{ID: ids.New(ids.Asset), ProjectID: db.DefaultProjectID, AppID: app.ID, StorageID: fsID})
	if err != nil {
		t.Fatal(err)
	}
	row, err := set.AddS3(ctx, db.DefaultProjectID, KindR2, cfg, sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.SetDefault(ctx, db.DefaultProjectID, row.ID); err != nil {
		t.Fatal(err)
	}
	newID, newStore, err := set.Default(ctx, db.DefaultProjectID)
	if err != nil || newID != row.ID {
		t.Fatalf("Default = %s, %v; want %s", newID, err, row.ID)
	}
	if st, err := set.ForAsset(ctx, old.ID); err != nil || st != storage.Storage(fs) {
		t.Fatalf("old asset's store = %T, %v; want the filesystem", st, err)
	}
	if _, ok := newStore.(*storage.S3); !ok {
		t.Fatalf("new default = %T, want S3", newStore)
	}
	if err := set.SetDefault(ctx, db.DefaultProjectID, "sto_none"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("SetDefault(unknown) = %v, want ErrUnknown", err)
	}
	if id, _, _ := set.Default(ctx, db.DefaultProjectID); id != row.ID {
		t.Fatalf("a failed SetDefault changed the default to %s", id)
	}
}
