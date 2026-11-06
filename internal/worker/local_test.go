package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalFilesMustBeRealFilesInsideTheRoots(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	video := filepath.Join(root, "lesson.mp4")
	secret := filepath.Join(outside, "id_ed25519")
	for _, p := range []string{video, secret} {
		if err := os.WriteFile(p, []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "innocent.mp4")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(root, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	if f, err := localFile(video, roots); err != nil || f.Size != 5 {
		t.Fatalf("a file in a root = %+v, %v", f, err)
	}
	for name, path := range map[string]string{
		"relative":                "lesson.mp4",
		"outside the roots":       secret,
		"a symlink out of a root": link,
		"a directory":             root,
		"empty":                   empty,
	} {
		if _, err := localFile(path, roots); err == nil {
			t.Errorf("%s (%s) was accepted", name, path)
		}
	}
	// Without roots, any file this user can read may be pushed.
	if _, err := localFile(secret, nil); err != nil {
		t.Fatalf("no roots: %v", err)
	}
}

func TestALocalSourceThatChangedOrVanishedIsRefused(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "lesson.mp4")
	if err := os.WriteFile(video, []byte("first take"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := localFile(video, nil)
	if err := s.add("src_1", f); err != nil {
		t.Fatal(err)
	}
	// The registry survives a restart.
	if s, err = OpenSources(dir); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Resolve("src_1", 10); err != nil || p != video {
		t.Fatalf("Resolve = %q, %v", p, err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(video, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("src_1", 10); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("after an edit: %v, want ErrSourceChanged", err)
	}
	if err := os.Remove(video); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve("src_1", 10); !errors.Is(err, ErrSourceMissing) {
		t.Fatalf("after removal: %v, want ErrSourceMissing", err)
	}
	if _, err := s.Resolve("src_unknown", 10); !errors.Is(err, ErrSourceMissing) {
		t.Fatalf("unknown source: %v, want ErrSourceMissing", err)
	}
	if code := localCode(ErrSourceChanged); code != "source_changed" || !permanent(code) {
		t.Fatalf("code %q permanent=%v", code, permanent(code))
	}
}
