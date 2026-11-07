// Package config reads the control plane's configuration from the
// environment. docs/configuration.md lists every variable and .env.example
// gives each an example; doc_test.go keeps both complete.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

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
	// SecretsKeyPrevious is the key being rotated away from
	// (CLIPMUX_SECRETS_KEY_PREVIOUS): values it sealed still open until
	// clipmux admin reseal-secrets seals them with SecretsKey. Never log it.
	SecretsKeyPrevious string
	// TrustedProxies are the addresses (e.g. Caddy's) whose X-Forwarded-For
	// is believed when limiting sign-ins (CLIPMUX_TRUSTED_PROXIES, comma-
	// separated addresses or CIDR ranges).
	TrustedProxies []netip.Prefix
	// UnsandboxedWorker runs the built-in worker even where the sandbox
	// doesn't hold (CLIPMUX_UNSANDBOXED_WORKER). Only for hosts that isolate
	// the worker another way: its tools parse hostile uploads.
	UnsandboxedWorker bool
	// SelfDial, when set (a host, or host:port), is where the setup test
	// connects for every URL it checks, keeping the URL's hostname for TLS
	// and routing (CLIPMUX_SELF_DIAL, e.g. caddy): a container can't always
	// reach its own public hostname. A bare host keeps each URL's port.
	SelfDial string
	// CloudflareAPI is Cloudflare's API base (CLIPMUX_CLOUDFLARE_API;
	// default the public v4 API), for an API proxy or a test double.
	CloudflareAPI string
	// R2Endpoint is the R2 S3 endpoint, {account_id} replaced by the
	// account (CLIPMUX_R2_ENDPOINT; default https://{account_id}.r2.cloudflarestorage.com),
	// e.g. the EU jurisdiction's https://{account_id}.eu.r2.cloudflarestorage.com.
	R2Endpoint string
	// RunPodAPI is RunPod's REST API base (CLIPMUX_RUNPOD_API; default the
	// public v1 API), for a test double.
	RunPodAPI string
	// WorkerImage is the image provider machines run
	// (CLIPMUX_WORKER_IMAGE), until builds carry their own (P4-T06).
	WorkerImage string
	// ExecutorsAllowPrivateURL lets a provider connect while the public URL
	// is private (CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL): only for development
	// and tests whose "provider" runs on the same machine.
	ExecutorsAllowPrivateURL bool
	// MetricsAddr is where /metrics is served (CLIPMUX_METRICS_ADDR,
	// host:port; unset, nowhere). It is a listener of its own, never the
	// public one, and needs no auth: keep it off the internet.
	MetricsAddr string
	// PProf adds Go's /debug/pprof/ handlers to the metrics listener
	// (CLIPMUX_PPROF).
	PProf bool
	// Version is the build's version, for clipmux_build_info; the binary
	// sets it, not the environment.
	Version string
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
		ListenAddr:         orDefault(getenv("CLIPMUX_LISTEN"), DefaultListenAddr),
		DatabaseURL:        getenv("DATABASE_URL"),
		DataDir:            orDefault(getenv("CLIPMUX_DATA_DIR"), DefaultDataDir),
		SecretsKey:         getenv("CLIPMUX_SECRETS_KEY"),
		SecretsKeyPrevious: getenv("CLIPMUX_SECRETS_KEY_PREVIOUS"),
		SelfDial:           getenv("CLIPMUX_SELF_DIAL"),
		CloudflareAPI:      getenv("CLIPMUX_CLOUDFLARE_API"),
		R2Endpoint:         getenv("CLIPMUX_R2_ENDPOINT"),
		RunPodAPI:          getenv("CLIPMUX_RUNPOD_API"),
		WorkerImage:        getenv("CLIPMUX_WORKER_IMAGE"),
		MetricsAddr:        getenv("CLIPMUX_METRICS_ADDR"),
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
	if cfg.BuiltinWorker, err = boolEnv(getenv, "CLIPMUX_BUILTIN_WORKER", true); err != nil {
		return Config{}, err
	}
	for _, p := range strings.FieldsFunc(getenv("CLIPMUX_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			addr, aerr := netip.ParseAddr(p)
			if aerr != nil {
				return Config{}, fmt.Errorf("CLIPMUX_TRUSTED_PROXIES: %q is not an address or CIDR range", p)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, prefix.Masked())
	}
	if cfg.SelfDial != "" && strings.ContainsAny(cfg.SelfDial, "/ ") {
		return Config{}, fmt.Errorf("CLIPMUX_SELF_DIAL %q must be a host or host:port", cfg.SelfDial)
	}
	if cfg.UnsandboxedWorker, err = boolEnv(getenv, "CLIPMUX_UNSANDBOXED_WORKER", false); err != nil {
		return Config{}, err
	}
	if cfg.ExecutorsAllowPrivateURL, err = boolEnv(getenv, "CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL", false); err != nil {
		return Config{}, err
	}
	if cfg.PProf, err = boolEnv(getenv, "CLIPMUX_PPROF", false); err != nil {
		return Config{}, err
	}
	// The built-in worker reads this itself; checking it here stops serve
	// at boot rather than leaving the worker restarting.
	if v := getenv("CLIPMUX_TOOL_MEMORY_MB"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err != nil || n == 0 {
			return Config{}, fmt.Errorf("CLIPMUX_TOOL_MEMORY_MB %q must be a positive number of MiB", v)
		}
	}
	if cfg.MetricsAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.MetricsAddr); err != nil {
			return Config{}, fmt.Errorf("CLIPMUX_METRICS_ADDR %q: %w", cfg.MetricsAddr, err)
		}
	}
	if cfg.PProf && cfg.MetricsAddr == "" {
		return Config{}, errors.New("CLIPMUX_PPROF needs CLIPMUX_METRICS_ADDR: pprof is served only on the metrics listener")
	}
	cfg.Tools = media.DefaultTools()
	cfg.Tools.FFmpeg = orDefault(getenv("CLIPMUX_FFMPEG"), cfg.Tools.FFmpeg)
	cfg.Tools.FFprobe = orDefault(getenv("CLIPMUX_FFPROBE"), cfg.Tools.FFprobe)
	cfg.Tools.Packager = orDefault(getenv("CLIPMUX_PACKAGER"), cfg.Tools.Packager)
	return cfg, nil
}

// boolEnv reads a true/false variable; unset is def.
func boolEnv(getenv func(string) string, name string, def bool) (bool, error) {
	switch v := getenv(name); v {
	case "":
		return def, nil
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s %q must be true or false", name, v)
	}
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
