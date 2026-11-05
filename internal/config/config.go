// Package config reads the control plane's configuration from the
// environment. Every variable is documented in .env.example.
package config

import (
	"errors"
	"fmt"
	"net"
)

// Config is the resolved process configuration.
type Config struct {
	// ListenAddr is where `clipmux serve` listens (CLIPMUX_LISTEN).
	ListenAddr string
	// DatabaseURL is the Postgres connection string (DATABASE_URL).
	DatabaseURL string
	// DataDir holds filesystem storage and local state (CLIPMUX_DATA_DIR).
	DataDir string
}

// Defaults applied when a variable is unset.
const (
	DefaultListenAddr = ":8080"
	DefaultDataDir    = "./data"
)

// FromEnv builds a Config from a getenv function (os.Getenv in production).
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:  orDefault(getenv("CLIPMUX_LISTEN"), DefaultListenAddr),
		DatabaseURL: getenv("DATABASE_URL"),
		DataDir:     orDefault(getenv("CLIPMUX_DATA_DIR"), DefaultDataDir),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return Config{}, fmt.Errorf("CLIPMUX_LISTEN %q: %w", cfg.ListenAddr, err)
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
