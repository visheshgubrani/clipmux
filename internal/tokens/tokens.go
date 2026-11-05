// Package tokens signs and verifies playback tokens (docs/architecture.md,
// "Tokens"): ES256 JWTs carried in the URL path. The app's backend signs with
// its private key; delivery verifies with the public key it registered. Only
// the standard library is used, and only ES256 is accepted, so there is no
// algorithm confusion to get wrong.
package tokens

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Audiences.
const (
	// AudiencePlayback fetches everything, including the content key.
	AudiencePlayback = "v"
	// AudienceThumbnail fetches only poster.jpg.
	AudienceThumbnail = "t"
)

// Errors from Parse and Verify.
var (
	ErrMalformed = errors.New("tokens: malformed token")
	ErrSignature = errors.New("tokens: bad signature")
)

// Claims are a playback token's claims.
type Claims struct {
	Sub string `json:"sub"`           // playback ID
	Aud string `json:"aud"`           // "v" or "t"
	Iat int64  `json:"iat"`           // issued at, Unix seconds
	Exp int64  `json:"exp"`           // expiry, Unix seconds
	Vid string `json:"vid,omitempty"` // optional viewer ID, for auditing
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ,omitempty"`
}

// b64 is strict: it rejects non-canonical encodings (nonzero padding bits),
// so a token has exactly one spelling.
var b64 = base64.RawURLEncoding.Strict()

// Sign returns a compact ES256 JWT.
func Sign(key *ecdsa.PrivateKey, kid string, c Claims) (string, error) {
	h, _ := json.Marshal(header{Alg: "ES256", Kid: kid, Typ: "JWT"})
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig), nil
}

// Token is a parsed, not yet verified token.
type Token struct {
	Kid    string
	Claims Claims
	input  string
	sig    []byte
}

// Parse decodes a token without verifying it, so the caller can look up the
// key named by its kid.
func Parse(token string) (Token, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 4096 {
		return Token{}, ErrMalformed
	}
	var h header
	if raw, err := b64.DecodeString(parts[0]); err != nil || json.Unmarshal(raw, &h) != nil {
		return Token{}, ErrMalformed
	}
	if h.Alg != "ES256" || h.Kid == "" {
		return Token{}, fmt.Errorf("%w: alg %q, kid %q", ErrMalformed, h.Alg, h.Kid)
	}
	var c Claims
	if raw, err := b64.DecodeString(parts[1]); err != nil || json.Unmarshal(raw, &c) != nil {
		return Token{}, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return Token{}, ErrMalformed
	}
	return Token{Kid: h.Kid, Claims: c, input: parts[0] + "." + parts[1], sig: sig}, nil
}

// Verify checks the signature with key. It does not check claims.
func (t Token) Verify(key *ecdsa.PublicKey) error {
	digest := sha256.Sum256([]byte(t.input))
	r := new(big.Int).SetBytes(t.sig[:32])
	s := new(big.Int).SetBytes(t.sig[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return ErrSignature
	}
	return nil
}

// GenerateKey returns a new P-256 key pair as PKCS#8 and PKIX PEM.
func GenerateKey() (privatePEM, publicPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})), nil
}

// ParsePublicKey reads a PKIX PEM P-256 public key.
func ParsePublicKey(publicPEM string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(publicPEM))
	if block == nil {
		return nil, errors.New("tokens: no PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("tokens: not a P-256 public key")
	}
	return ec, nil
}

// ParsePrivateKey reads a PKCS#8 PEM P-256 private key.
func ParsePrivateKey(privatePEM string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("tokens: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("tokens: not a P-256 private key")
	}
	return ec, nil
}
