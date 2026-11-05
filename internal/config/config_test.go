package config

import "testing"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.DataDir != "./data" || cfg.PublicURL != "http://localhost:8080" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestPublicURLIsAnOriginOnly(t *testing.T) {
	ok := map[string]string{
		"https://video.example.com":  "https://video.example.com",
		"https://video.example.com/": "https://video.example.com",
		"http://192.168.1.5:8080":    "http://192.168.1.5:8080",
	}
	for in, want := range ok {
		cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_PUBLIC_URL": in}))
		if err != nil || cfg.PublicURL != want {
			t.Errorf("CLIPMUX_PUBLIC_URL=%q: got %q, %v; want %q", in, cfg.PublicURL, err, want)
		}
	}
	// Presigned URLs and base_url are built on it, so it must be a bare origin.
	for _, bad := range []string{"video.example.com", "ftp://x.example", "https://x.example/app", "https://x.example?a=1", "https://"} {
		if _, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_PUBLIC_URL": bad})); err == nil {
			t.Errorf("CLIPMUX_PUBLIC_URL=%q: want an error", bad)
		}
	}
}

func TestFromEnvRequiresDatabaseURL(t *testing.T) {
	if _, err := FromEnv(env(nil)); err == nil {
		t.Fatal("want error without DATABASE_URL")
	}
}

func TestFromEnvRejectsBadListenAddr(t *testing.T) {
	_, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_LISTEN": "8080"}))
	if err == nil {
		t.Fatal("want error for listen address without a colon")
	}
}

func TestMaxUploadBytes(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_MAX_UPLOAD_BYTES": "26843545600"}))
	if err != nil || cfg.MaxUploadBytes != 26843545600 {
		t.Fatalf("got %d, %v", cfg.MaxUploadBytes, err)
	}
	for _, bad := range []string{"0", "-1", "25GiB"} {
		if _, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_MAX_UPLOAD_BYTES": bad})); err == nil {
			t.Errorf("CLIPMUX_MAX_UPLOAD_BYTES=%q: want an error", bad)
		}
	}
}
