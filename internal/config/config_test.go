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

func TestBuiltinWorkerAndTools(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil || !cfg.BuiltinWorker || cfg.Tools.Packager != "packager" {
		t.Fatalf("defaults: %+v, %v", cfg, err)
	}
	cfg, err = FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_BUILTIN_WORKER": "false", "CLIPMUX_PACKAGER": "/opt/shaka/packager"}))
	if err != nil || cfg.BuiltinWorker || cfg.Tools.Packager != "/opt/shaka/packager" {
		t.Fatalf("overrides: %+v, %v", cfg, err)
	}
	if _, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_BUILTIN_WORKER": "yes"})); err == nil {
		t.Fatal("an unclear boolean must be refused")
	}
}

func TestMediaURLDefaultsToThePublicURL(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_PUBLIC_URL": "https://video.example.com"}))
	if err != nil || cfg.MediaURL != "https://video.example.com" {
		t.Fatalf("got %q, %v", cfg.MediaURL, err)
	}
	cfg, err = FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_PUBLIC_URL": "https://video.example.com", "CLIPMUX_MEDIA_URL": "https://media.example.net/"}))
	if err != nil || cfg.MediaURL != "https://media.example.net" {
		t.Fatalf("got %q, %v", cfg.MediaURL, err)
	}
	if _, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_PUBLIC_URL": "https://video.example.com", "CLIPMUX_MEDIA_URL": "https://m.example.net/path"})); err == nil {
		t.Fatal("a media URL with a path must be refused")
	}
}

// Setting CLIPMUX_PUBLIC_URL pins both URLs: the first-boot URL step shows
// them read-only. A media URL alone has no public URL to belong to.
func TestURLsArePinnedOnlyWhenSetInTheEnvironment(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil || cfg.URLsPinned {
		t.Fatalf("defaults: pinned = %v, %v; want unpinned", cfg.URLsPinned, err)
	}
	cfg, err = FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_PUBLIC_URL": "https://video.example.com"}))
	if err != nil || !cfg.URLsPinned || cfg.MediaURL != "https://video.example.com" {
		t.Fatalf("public URL set: %+v, %v; want pinned with media = public", cfg, err)
	}
	if _, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_MEDIA_URL": "https://media.example.net"})); err == nil {
		t.Fatal("CLIPMUX_MEDIA_URL without CLIPMUX_PUBLIC_URL must be refused")
	}
}

func TestTrustedProxiesAreAddressRanges(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_TRUSTED_PROXIES": "172.16.0.0/12, 10.0.0.5"}))
	if err != nil || len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0].String() != "172.16.0.0/12" || cfg.TrustedProxies[1].String() != "10.0.0.5/32" {
		t.Fatalf("got %v, %v", cfg.TrustedProxies, err)
	}
	if _, err := FromEnv(env(map[string]string{"DATABASE_URL": "postgres://x", "CLIPMUX_TRUSTED_PROXIES": "caddy"})); err == nil {
		t.Fatal("a hostname is not an address range")
	}
}

type envCase struct {
	env map[string]string
	ok  bool
}

func checkEnv(t *testing.T, cases []envCase) {
	t.Helper()
	for _, c := range cases {
		get := func(k string) string {
			if v, ok := c.env[k]; ok {
				return v
			}
			if k == "DATABASE_URL" {
				return "postgres://x"
			}
			return ""
		}
		if _, err := FromEnv(get); (err == nil) != c.ok {
			t.Errorf("%v: err %v, want ok=%v", c.env, err, c.ok)
		}
	}
}

func TestMetricsAndPProfAreChecked(t *testing.T) {
	checkEnv(t, []envCase{
		{map[string]string{"CLIPMUX_METRICS_ADDR": "127.0.0.1:9090", "CLIPMUX_PPROF": "true"}, true},
		{map[string]string{"CLIPMUX_METRICS_ADDR": "9090"}, false},
		{map[string]string{"CLIPMUX_PPROF": "true"}, false}, // pprof only on the metrics listener
		{map[string]string{"CLIPMUX_PPROF": "yes"}, false},
	})
}

func TestToolMemoryIsChecked(t *testing.T) {
	checkEnv(t, []envCase{
		{map[string]string{"CLIPMUX_TOOL_MEMORY_MB": "4096"}, true},
		{map[string]string{"CLIPMUX_TOOL_MEMORY_MB": "4G"}, false},
		{map[string]string{"CLIPMUX_TOOL_MEMORY_MB": "0"}, false},
	})
}
