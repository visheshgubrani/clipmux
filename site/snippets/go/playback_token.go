package clipmux

import (
	"crypto/ecdsa"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// PlaybackToken signs a token for a signed playback ID with your app's
// signing key: CLIPMUX_SIGNING_KEY, read once with
// jwt.ParseECPrivateKeyFromPEM, and CLIPMUX_SIGNING_KID. audience is "v" to
// play, or "t" for the poster. viewerID may be empty; when set it counts
// unique viewers, and ClipMux keeps only a keyed hash of it.
func PlaybackToken(key *ecdsa.PrivateKey, kid, playbackID, viewerID, audience string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{"sub": playbackID, "aud": audience, "iat": now.Unix(), "exp": now.Add(ttl).Unix()}
	if viewerID != "" {
		claims["vid"] = viewerID
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = kid
	return t.SignedString(key)
}
