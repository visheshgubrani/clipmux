package clipmux

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The Go snippets against the shared vector files.

func load(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Name, Secret, Body, Reason string
			Now                        int64
			Headers                    map[string]string
			Valid                      bool
		}
	}
	load(t, "internal/webhooks/testdata/vectors.json", &v)
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.Cases {
		h := http.Header{}
		for k, val := range c.Headers {
			h.Set(k, val)
		}
		err := VerifyWebhook(c.Secret, []byte(c.Body), h, time.Unix(c.Now, 0))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if (err == nil) != c.Valid || got != c.Reason {
			t.Errorf("%s: got %q, want valid=%v reason=%q", c.Name, got, c.Valid, c.Reason)
		}
	}
}

func TestPlaybackTokenIsWhatDeliveryReads(t *testing.T) {
	var v struct {
		MaxLifetime int64 `json:"max_token_lifetime_seconds"`
		SigningKeys []struct {
			Kid        string `json:"kid"`
			PrivatePEM string `json:"private_key_pem"`
			PublicPEM  string `json:"public_key_pem"`
		} `json:"signing_keys"`
		TokenClaims map[string]struct {
			Kid, Sub, Aud, Vid string
		} `json:"token_claims"`
	}
	load(t, "internal/delivery/testdata/vectors.json", &v)
	want := v.TokenClaims["v"]
	var privPEM, pubPEM string
	for _, k := range v.SigningKeys {
		if k.Kid == want.Kid {
			privPEM, pubPEM = k.PrivatePEM, k.PublicPEM
		}
	}
	key, err := jwt.ParseECPrivateKeyFromPEM([]byte(privPEM))
	if err != nil {
		t.Fatal(err)
	}
	token, err := PlaybackToken(key, want.Kid, want.Sub, want.Vid, "v", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 || strings.Contains(token, "=") {
		t.Fatalf("not a compact JWT: %s", token)
	}
	var head map[string]string
	raw, _ := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err := json.Unmarshal(raw, &head); err != nil || head["alg"] != "ES256" || head["kid"] != want.Kid || head["typ"] != "JWT" {
		t.Fatalf("header %s", raw)
	}
	if sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[2]); err != nil || len(sig) != 64 {
		t.Fatalf("signature is %d bytes, %v", len(sig), err)
	}

	pub, err := jwt.ParseECPublicKeyFromPEM([]byte(pubPEM))
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return pub, nil },
		jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("v")); err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != want.Sub || claims["aud"] != want.Aud || claims["vid"] != want.Vid {
		t.Errorf("claims %v, want %+v", claims, want)
	}
	life := int64(claims["exp"].(float64) - claims["iat"].(float64))
	if life != 900 || life > v.MaxLifetime {
		t.Errorf("lifetime %d s", life)
	}
	if !errors.Is(VerifyWebhook("", nil, http.Header{}, time.Now()), ErrMissingHeader) {
		t.Error("no headers is not missing_header")
	}
}
