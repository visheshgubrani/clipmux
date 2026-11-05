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
	if cfg.ListenAddr != ":8080" || cfg.DataDir != "./data" {
		t.Fatalf("defaults not applied: %+v", cfg)
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
