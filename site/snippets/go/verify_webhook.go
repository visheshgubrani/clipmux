package clipmux

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Answer 400 for any of these.
var (
	ErrMissingHeader = errors.New("missing_header")
	ErrStale         = errors.New("stale")
	ErrBadSignature  = errors.New("bad_signature")
)

// VerifyWebhook checks a ClipMux webhook. secret is CLIPMUX_WEBHOOK_SECRET,
// whsec_ prefix included, and body is the request body exactly as it
// arrived. Unmarshal the body into the event only after it passes.
func VerifyWebhook(secret string, body []byte, h http.Header, now time.Time) error {
	ts, sig := h.Get("X-Webhook-Timestamp"), h.Get("X-Webhook-Signature")
	if ts == "" || sig == "" {
		return ErrMissingHeader
	}
	for _, r := range ts {
		if r < '0' || r > '9' {
			return ErrStale
		}
	}
	sent, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Unix()-sent > 300 || sent-now.Unix() > 300 {
		return ErrStale
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	if !hmac.Equal([]byte(sig), []byte("sha256="+hex.EncodeToString(mac.Sum(nil)))) {
		return ErrBadSignature
	}
	return nil
}
