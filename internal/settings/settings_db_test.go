package settings

import (
	"context"
	"errors"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

func TestEnvPinsTheURLsAndTheStepIsReadOnly(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	cfg := config.Config{PublicURL: "https://video.example.com", MediaURL: "https://media.example.net", URLsPinned: true}
	s, err := Load(ctx, pool, "prj_default", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := URLs{Public: "https://video.example.com", Media: "https://media.example.net", Confirmed: true, Pinned: true}
	if got := s.URLs(); got != want {
		t.Fatalf("URLs = %+v, want %+v", got, want)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT media_url FROM projects WHERE id = 'prj_default'`).Scan(&stored); err != nil || stored != "https://media.example.net" {
		t.Fatalf("stored media_url = %q, %v; base_url is read from the database", stored, err)
	}
	if _, err := s.Set(ctx, "https://other.example.com", ""); !errors.Is(err, ErrPinned) {
		t.Fatalf("Set while pinned = %v, want ErrPinned", err)
	}
}

func TestSavedURLsApplyWithoutRestart(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	cfg := config.Config{PublicURL: config.DefaultPublicURL, MediaURL: config.DefaultPublicURL}
	s, err := Load(ctx, pool, "prj_default", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.URLs(); got != (URLs{Public: "http://localhost:8080", Media: "http://localhost:8080"}) {
		t.Fatalf("first boot URLs = %+v, want the unconfirmed defaults", got)
	}
	var seen []URLs
	s.OnChange(func(u URLs) { seen = append(seen, u) })
	if _, err := s.Set(ctx, "https://video.example.com/", ""); err != nil {
		t.Fatal(err)
	}
	want := URLs{Public: "https://video.example.com", Media: "https://video.example.com", Confirmed: true}
	if got := s.URLs(); got != want || len(seen) != 1 || seen[0] != want {
		t.Fatalf("after Set: %+v (hooks saw %v), want %+v", got, seen, want)
	}
	again, err := Load(ctx, pool, "prj_default", cfg)
	if err != nil || again.URLs() != want {
		t.Fatalf("reload = %+v, %v; want the saved URLs", again.URLs(), err)
	}
	for _, bad := range [][2]string{{"ftp://x.example.com", ""}, {"https://x.example.com/path", ""}, {"https://x.example.com", "media.example.com"}} {
		if _, err := s.Set(ctx, bad[0], bad[1]); err == nil {
			t.Errorf("Set(%q, %q) succeeded, want an origin error", bad[0], bad[1])
		}
	}
	if !s.ServesHost("video.example.com") || s.ServesHost("evil.example.com") || s.ServesHost("") {
		t.Fatal("ServesHost must answer only for the configured hostnames")
	}
}
