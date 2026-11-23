package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
)

func TestConfigReadsTheTargetAndItsDefaults(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	dir := t.TempDir()

	def, err := FromEnv(env(nil), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !def.Local || def.URL != "file://"+dir+"/backups" || def.Interval != DefaultLocalInterval {
		t.Errorf("default = %+v, want the local directory, once a day", def)
	}
	if def.Policy() != (Policy{Base: 7 * 24 * 3600e9}) {
		t.Errorf("local policy = %+v, want seven days of one tier", def.Policy())
	}

	s3, err := FromEnv(env(map[string]string{
		"CLIPMUX_BACKUP_URL": "s3://vault/clipmux/prod?endpoint=https://s3.us-west-004.backblazeb2.com&region=us-west-004",
	}), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Local || s3.Interval != DefaultInterval || s3.Policy().Monthly == 0 {
		t.Errorf("s3 = %+v, want an hourly target with every tier", s3)
	}

	for name, m := range map[string]map[string]string{
		"a scheme we don't speak": {"CLIPMUX_BACKUP_URL": "ftp://x/y"},
		"a relative file":         {"CLIPMUX_BACKUP_URL": "file://backups"},
		"a bucket path with dots": {"CLIPMUX_BACKUP_URL": "s3://b/../x"},
		"an interval that isn't":  {"CLIPMUX_BACKUP_INTERVAL": "often"},
		"an interval of seconds":  {"CLIPMUX_BACKUP_INTERVAL": "10s"},
		"a recipient that isn't":  {"CLIPMUX_BACKUP_RECIPIENTS": "ssh-ed25519 AAAA"},
		"a ping that isn't a URL": {"CLIPMUX_BACKUP_PING_URL": "hc-ping"},
		"an endpoint that isn't":  {"CLIPMUX_BACKUP_URL": "s3://b/p?endpoint=ftp://x"},
		"a bucket-less s3 target": {"CLIPMUX_BACKUP_URL": "s3:///p"},
	} {
		if _, err := FromEnv(env(m), dir); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if !def.BeforeMigrate {
		t.Error("a backup before migrating is off by default; it should be on")
	}
	for v, want := range map[string]bool{"true": true, "false": false, "0": false, "1": true} {
		c, err := FromEnv(env(map[string]string{"CLIPMUX_BACKUP_BEFORE_MIGRATE": v}), dir)
		if err != nil || c.BeforeMigrate != want {
			t.Errorf("CLIPMUX_BACKUP_BEFORE_MIGRATE=%s: %v, %v, want %v", v, c.BeforeMigrate, err, want)
		}
	}
	if _, err := FromEnv(env(map[string]string{"CLIPMUX_BACKUP_BEFORE_MIGRATE": "maybe"}), dir); err == nil {
		t.Error("CLIPMUX_BACKUP_BEFORE_MIGRATE=maybe was accepted")
	}
	if off, err := FromEnv(env(map[string]string{"CLIPMUX_BACKUP_INTERVAL": "0"}), dir); err != nil || off.Interval != 0 {
		t.Errorf("0 turns scheduled backups off: %+v, %v", off, err)
	}
}

func TestAnS3TargetNeedsCredentials(t *testing.T) {
	_, err := OpenTarget(Config{URL: "s3://vault/p?endpoint=http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "CLIPMUX_BACKUP_ACCESS_KEY_ID") {
		t.Fatalf("err = %v, want it to name the credentials", err)
	}
}

func TestADirectoryTargetKeepsItsKeysUnderThePrefixAndProbesAsDeletable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tg, err := OpenTarget(Config{URL: "file://" + dir})
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	p, err := tg.Probe(ctx)
	if err != nil || !p.CanDelete {
		t.Fatalf("Probe = %+v, %v, want a directory we can delete from", p, err)
	}
	if err := tg.Put(ctx, "db/hourly/a.json", strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	objs, err := tg.list(ctx, "db/hourly/")
	if err != nil || len(objs) != 1 || objs[0].Key != "db/hourly/a.json" {
		t.Fatalf("list = %+v, %v", objs, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects", "db", "hourly", "a.json")); err != nil {
		t.Errorf("the object isn't where the filesystem adapter keeps it: %v", err)
	}
}

// A key that can write and read but not delete, as the plan advises for B2,
// is reported, not rejected.
func TestATargetThatRefusesDeletesProbesAsImmutable(t *testing.T) {
	b := s3test.New(t)
	tg, err := OpenTarget(Config{
		URL:         "s3://" + b.Name + "/vault?endpoint=" + b.Endpoint,
		AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := tg.Probe(context.Background())
	if err != nil || !p.CanDelete {
		t.Fatalf("Probe on a bucket we may delete from = %+v, %v", p, err)
	}
	locked := &Target{store: noDelete{tg.store}, prefix: tg.prefix, Desc: tg.Desc}
	p, err = locked.Probe(context.Background())
	if err != nil || p.CanDelete {
		t.Fatalf("Probe with deletes refused = %+v, %v, want immutable and no error", p, err)
	}
}

type noDelete struct{ storage.Storage }

func (noDelete) Delete(context.Context, string) error       { return errDenied }
func (noDelete) DeletePrefix(context.Context, string) error { return errDenied }

var errDenied = os.ErrPermission

// On B2, through the S3 API, a key without deleteFiles may still hide a file,
// so a delete is accepted; on a versioned bucket it only adds a marker. The
// bucket's Object Lock is what keeps the backups, and ClipMux mustn't prune
// a locked bucket: it would only hide what the lock keeps anyway.
func TestALockedBucketProbesAsImmutableEvenWhenItAcceptsADelete(t *testing.T) {
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	probe := func(st storage.Storage) Probe {
		t.Helper()
		p, err := (&Target{store: st, Desc: "s3://vault/"}).Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	if p := probe(lockState{Storage: fs, locked: true}); !p.CanDelete || !p.Locked || p.Prunes() {
		t.Errorf("locked bucket = %+v, prunes %v; want locked and not pruned by ClipMux", p, p.Prunes())
	}
	if p := probe(lockState{Storage: fs}); p.Locked || !p.Prunes() {
		t.Errorf("unlocked bucket = %+v, prunes %v; want ClipMux to prune", p, p.Prunes())
	}
	// A key that may not read the lock: reported, and the delete alone decides.
	if p := probe(lockState{Storage: fs, err: errDenied}); p.LockErr == nil || p.Locked || !p.Prunes() {
		t.Errorf("unreadable lock = %+v, prunes %v", p, p.Prunes())
	}
	// A directory target has no lock to read.
	if p := probe(fs); p.Locked || p.LockErr != nil || !p.Prunes() {
		t.Errorf("directory = %+v", p)
	}
}

type lockState struct {
	storage.Storage
	locked bool
	err    error
}

func (l lockState) ObjectLock(context.Context) (bool, error) { return l.locked, l.err }
