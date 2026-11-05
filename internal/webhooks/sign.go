// Package webhooks delivers asset events to the endpoints each app
// registers (docs/architecture.md, "Webhooks"): one delivery per endpoint,
// at least once and unordered, signed so a receiver can reject forgeries and
// replays.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Sign returns the X-Webhook-Signature value for a delivery: "sha256=" and
// the hex HMAC-SHA256 of "{timestamp}.{body}", keyed with the endpoint
// secret's bytes as shown to the owner (whsec_…). It is the scheme the LMS
// already verifies, with timestamp in Unix seconds.
func Sign(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(strconv.AppendInt(nil, timestamp, 10))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
