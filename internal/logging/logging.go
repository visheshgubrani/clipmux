// Package logging builds the process logger: text or JSON, a level, the
// request and job attributes carried by a context, and a last line of
// redaction so no credential reaches a log.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	neturl "net/url"
	"regexp"
	"strings"
	"time"
)

// Environment variables, read by every command that logs.
const (
	EnvFormat = "CLIPMUX_LOG_FORMAT" // text (default) or json
	EnvLevel  = "CLIPMUX_LOG_LEVEL"  // debug, info (default), warn or error
)

// FromEnv builds the logger the environment asks for. An invalid value is an
// error, and the returned logger is still usable (text, info) to report it.
func FromEnv(getenv func(string) string, w io.Writer) (*slog.Logger, error) {
	return New(w, getenv(EnvFormat), getenv(EnvLevel))
}

// New builds a logger writing to w. format is "text" or "json" and level
// "debug", "info", "warn" or "error"; empty strings take the defaults.
func New(w io.Writer, format, level string) (*slog.Logger, error) {
	var errs []error
	lvl := slog.LevelInfo
	if level != "" {
		if err := lvl.UnmarshalText([]byte(level)); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: want debug, info, warn or error", EnvLevel, level))
			lvl = slog.LevelInfo
		}
	}
	opts := &slog.HandlerOptions{Level: lvl, ReplaceAttr: replaceAttr}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "", "text":
		h = slog.NewTextHandler(w, opts)
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		errs = append(errs, fmt.Errorf("%s=%q: want text or json", EnvFormat, format))
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(contextHandler{h}), errors.Join(errs...)
}

// --- attributes carried by a context ---

type ctxKey struct{}

// With returns a context whose log lines carry these attributes too, as
// alternating keys and values like slog's: a request ID, a job, an attempt.
func With(ctx context.Context, args ...any) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	r := slog.NewRecord(time.Time{}, 0, "", 0)
	r.Add(args...)
	attrs := make([]slog.Attr, len(prev), len(prev)+r.NumAttrs())
	copy(attrs, prev)
	r.Attrs(func(a slog.Attr) bool { attrs = append(attrs, a); return true })
	return context.WithValue(ctx, ctxKey{}, attrs)
}

// Attr returns the value of a context attribute, or "".
func Attr(ctx context.Context, key string) string {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	for i := len(attrs) - 1; i >= 0; i-- {
		if attrs[i].Key == key {
			return attrs[i].Value.String()
		}
	}
	return ""
}

// contextHandler adds the context's attributes to every record logged with
// a context (ErrorContext, InfoContext, …).
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr); len(attrs) > 0 {
		r = r.Clone()
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// --- redaction ---

// RedactURL drops a URL's query, which may carry a signature, the token
// of a /v/{token}/ playback path and the secret of a /s/{secret} share link. A value that doesn't parse is replaced.
func RedactURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return "REDACTED"
	}
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.User = nil
	u.Fragment = ""
	u.Path = RedactPath(u.Path)
	u.RawPath = ""
	return u.String()
}

// RedactPath hides the playback token of a /v/{token}/ path and the
// secret of a share link's /s/{secret} path (D104).
func RedactPath(p string) string {
	for _, prefix := range []string{"/v/", "/s/"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			if _, after, ok := strings.Cut(rest, "/"); ok {
				return prefix + "REDACTED/" + after
			}
			return prefix + "REDACTED"
		}
	}
	return p
}

// RedactError rewrites a URL error so its URL is redacted, keeping the
// operation and the cause. Other errors are returned as they are.
func RedactError(err error) error {
	var ue *neturl.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	return fmt.Errorf("%s %s: %w", ue.Op, RedactURL(ue.URL), ue.Err)
}

// URLHost is a URL's host alone, for logging where a fetch went.
func URLHost(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// Scrub is the last line of redaction, applied to every string a logger
// writes: credentials with ClipMux's prefixes, JWTs, playback tokens in
// paths and URL query strings are replaced, whatever logged them.
func Scrub(s string) string {
	if !strings.ContainsAny(s, "_/.?") {
		return s
	}
	for _, r := range scrubbers {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

var scrubbers = []struct {
	re   *regexp.Regexp
	with string
}{
	// API keys (cmx_), agent keys (cmxa_), dispatch keys (cmxd_), webhook
	// secrets (whsec_).
	{regexp.MustCompile(`\b(cmx[ad]?_|whsec_)[A-Za-z0-9_-]+`), "${1}REDACTED"},
	// JWTs: playback tokens and anything else signed.
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), "REDACTED"},
	// A playback token in a /v/{token}/ path.
	{regexp.MustCompile(`/v/[^/\s"?{][^/\s"?]*/`), "/v/REDACTED/"},
	// A share link's secret in a /s/{secret} path, with or without more after it.
	{regexp.MustCompile(`/s/[^/\s"?{][^/\s"?#]*`), "/s/REDACTED"},
	// URL query strings: presigned signatures (X-Amz-Signature, sig=).
	{regexp.MustCompile(`(https?://[^\s?"#]+)\?[^\s"#]*`), "${1}?REDACTED"},
	{regexp.MustCompile(`([?&](?:X-Amz-[A-Za-z-]+|sig|exp|token|code)=)[^&\s"#]*`), "${1}REDACTED"},
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); s != Scrub(s) {
			a.Value = slog.StringValue(Scrub(s))
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(Scrub(err.Error()))
		}
	}
	return a
}
