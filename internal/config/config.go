// Package config reads the control plane's configuration from the
// environment. Every variable is documented in .env.example.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/visheshgubrani/clipmux/internal/media"
)

// Config is the resolved process configuration.
type Config struct {
	// ListenAddr is where `clipmux serve` listens (CLIPMUX_LISTEN).
	ListenAddr string
	// DatabaseURL is the Postgres connection string (DATABASE_URL).
	DatabaseURL string
	// DataDir holds filesystem storage and local state (CLIPMUX_DATA_DIR).
	DataDir string
	// PublicURL is the origin browsers and workers reach the app at
	// (CLIPMUX_PUBLIC_URL). Signed storage URLs are built on it, so a URL
	// signed for an internal hostname would fail in a browser.
	PublicURL string
	// MediaURL is the origin playback is served from (CLIPMUX_MEDIA_URL),
	// returned to apps as base_url. Defaults to PublicURL; ideally a
	// different registrable domain from the dashboard.
	MediaURL string
	// URLsPinned is set when CLIPMUX_PUBLIC_URL is: the environment then
	// decides both URLs and the first-boot URL step is read-only. Unset,
	// the URLs saved at first boot apply (internal/settings).
	URLsPinned bool
	// MaxUploadBytes caps a declared upload size (CLIPMUX_MAX_UPLOAD_BYTES);
	// 0 means the storage limit (32 MiB × 10,000 parts).
	MaxUploadBytes int64
	// BuiltinWorker starts `clipmux worker` as a child of serve
	// (CLIPMUX_BUILTIN_WORKER, default true).
	BuiltinWorker bool
	// SecretsKey seals the secrets kept in Postgres (CLIPMUX_SECRETS_KEY,
	// "v1:" and 64 hex digits). Unset, serve keeps it in
	// {DataDir}/secrets.key, created on first boot. Never log it.
	SecretsKey string
	// Tools are the media binaries (CLIPMUX_FFMPEG, CLIPMUX_FFPROBE,
	// CLIPMUX_PACKAGER; default: found on PATH).
	Tools media.Tools
}

// Defaults applied when a variable is unset.
const (
	DefaultListenAddr = ":8080"
	DefaultDataDir    = "./data"
	DefaultPublicURL  = "http://localhost:8080"
)

// FromEnv builds a Config from a getenv function (os.Getenv in production).
func FromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:  orDefault(getenv("CLIPMUX_LISTEN"), DefaultListenAddr),
		DatabaseURL: getenv("DATABASE_URL"),
		DataDir:     orDefault(getenv("CLIPMUX_DATA_DIR"), DefaultDataDir),
		SecretsKey:  getenv("CLIPMUX_SECRETS_KEY"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return Config{}, fmt.Errorf("CLIPMUX_LISTEN %q: %w", cfg.ListenAddr, err)
	}
	publicURL, err := Origin(orDefault(getenv("CLIPMUX_PUBLIC_URL"), DefaultPublicURL))
	if err != nil {
		return Config{}, fmt.Errorf("CLIPMUX_PUBLIC_URL: %w", err)
	}
	cfg.PublicURL = publicURL
	cfg.URLsPinned = getenv("CLIPMUX_PUBLIC_URL") != ""
	if !cfg.URLsPinned && getenv("CLIPMUX_MEDIA_URL") != "" {
		return Config{}, errors.New("CLIPMUX_MEDIA_URL needs CLIPMUX_PUBLIC_URL; set both, or neither and choose them at first boot")
	}
	if cfg.MediaURL, err = Origin(orDefault(getenv("CLIPMUX_MEDIA_URL"), publicURL)); err != nil {
		return Config{}, fmt.Errorf("CLIPMUX_MEDIA_URL: %w", err)
	}
	if v := getenv("CLIPMUX_MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("CLIPMUX_MAX_UPLOAD_BYTES %q must be a positive number of bytes", v)
		}
		cfg.MaxUploadBytes = n
	}
	switch v := getenv("CLIPMUX_BUILTIN_WORKER"); v {
	case "", "true", "1":
		cfg.BuiltinWorker = true
	case "false", "0":
	default:
		return Config{}, fmt.Errorf("CLIPMUX_BUILTIN_WORKER %q must be true or false", v)
	}
	cfg.Tools = media.DefaultTools()
	cfg.Tools.FFmpeg = orDefault(getenv("CLIPMUX_FFMPEG"), cfg.Tools.FFmpeg)
	cfg.Tools.FFprobe = orDefault(getenv("CLIPMUX_FFPROBE"), cfg.Tools.FFprobe)
	cfg.Tools.Packager = orDefault(getenv("CLIPMUX_PACKAGER"), cfg.Tools.Packager)
	return cfg, nil
}

// Origin accepts an http(s) URL with no path, query or fragment and returns
// it without a trailing slash.
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q must be an http or https URL", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("%q must be an origin with no path, query or credentials", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
