package delivery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

var update = flag.Bool("update", false, "regenerate test keys and tokens in testdata/vectors.json")

const vectorsFile = "testdata/vectors.json"

type vectors struct {
	Description             string             `json:"description"`
	Now                     int64              `json:"now"`
	ContentKeySecretHex     string             `json:"content_key_secret_hex"`
	ContentKeyVersion       int32              `json:"content_key_version"`
	MaxTokenLifetimeSeconds int                `json:"max_token_lifetime_seconds"`
	Apps                    []string           `json:"apps"`
	SigningKeys             []vKey             `json:"signing_keys"`
	Assets                  []vAsset           `json:"assets"`
	Outputs                 []vOutput          `json:"outputs"`
	PlaybackIDs             []vPlaybackID      `json:"playback_ids"`
	Files                   map[string]string  `json:"files"`
	TokenClaims             map[string]vClaims `json:"token_claims"`
	Tokens                  map[string]string  `json:"tokens,omitempty"`
	Cases                   []vCase            `json:"cases"`
}

type vKey struct {
	Kid           string `json:"kid"`
	AppID         string `json:"app_id"`
	Revoked       bool   `json:"revoked,omitempty"`
	PublicKeyPEM  string `json:"public_key_pem,omitempty"`
	PrivateKeyPEM string `json:"private_key_pem,omitempty"`
}

type vAsset struct {
	ID, AppID, Status, CurrentOutputID string
}

func (a *vAsset) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID              string `json:"id"`
		AppID           string `json:"app_id"`
		Status          string `json:"status"`
		CurrentOutputID string `json:"current_output_id"`
	}
	err := json.Unmarshal(b, &raw)
	*a = vAsset{raw.ID, raw.AppID, raw.Status, raw.CurrentOutputID}
	return err
}

func (a vAsset) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"id": a.ID, "app_id": a.AppID, "status": a.Status, "current_output_id": a.CurrentOutputID})
}

type vOutput struct {
	ID         string `json:"id"`
	AssetID    string `json:"asset_id"`
	Status     string `json:"status"`
	KeyVersion int32  `json:"key_version"`
}

type vPlaybackID struct {
	ID      string `json:"id"`
	AssetID string `json:"asset_id"`
	Policy  string `json:"policy"`
	Status  string `json:"status"`
}

type vClaims struct {
	Kid string `json:"kid"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

type vCase struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Status       int    `json:"status"`
	ContentType  string `json:"content_type,omitempty"`
	CacheControl string `json:"cache_control,omitempty"`
	Body         string `json:"body,omitempty"`
	BodyHex      string `json:"body_hex,omitempty"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// regenerate fills in test keys (if missing) and re-signs every token.
func regenerate(t *testing.T, v *vectors) {
	t.Helper()
	privates := map[string]string{}
	for i, k := range v.SigningKeys {
		if k.PrivateKeyPEM == "" {
			priv, pub, err := tokens.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			v.SigningKeys[i].PrivateKeyPEM, v.SigningKeys[i].PublicKeyPEM = priv, pub
		}
		privates[k.Kid] = v.SigningKeys[i].PrivateKeyPEM
	}
	// A key nobody registered, for the unknown-kid case.
	stray, _, _ := tokens.GenerateKey()
	v.Tokens = map[string]string{}
	for name, c := range v.TokenClaims {
		pem := privates[c.Kid]
		if pem == "" {
			pem = stray
		}
		priv, err := tokens.ParsePrivateKey(pem)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := tokens.Sign(priv, c.Kid, tokens.Claims{Sub: c.Sub, Aud: c.Aud, Iat: c.Iat, Exp: c.Exp})
		if err != nil {
			t.Fatal(err)
		}
		v.Tokens[name] = tok
	}
	// Flip one signature byte of a valid token.
	sig := []byte(v.Tokens["v"])
	sig[len(sig)-5] ^= 1
	if sig[len(sig)-5] == '-' || sig[len(sig)-5] == '_' {
		sig[len(sig)-5] = 'A'
	}
	v.Tokens["tampered"] = string(sig)
	// The same signature spelled differently: the last character's unused
	// low bits changed. Delivery must accept exactly one spelling.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	canonical := v.Tokens["v"]
	last := strings.IndexByte(alphabet, canonical[len(canonical)-1])
	v.Tokens["noncanonical"] = canonical[:len(canonical)-1] + string(alphabet[last^1])
	raw, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(vectorsFile, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVectors(t *testing.T) {
	v := loadVectors(t)
	if *update {
		regenerate(t, &v)
	}
	if len(v.Tokens) == 0 {
		t.Fatal("testdata/vectors.json has no tokens; run with -update")
	}
	ctx := context.Background()
	pool := dbtest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`UPDATE projects SET max_token_lifetime_seconds = $1`, v.MaxTokenLifetimeSeconds)
	for _, app := range v.Apps {
		exec(`INSERT INTO apps (id, project_id, name) VALUES ($1, 'prj_default', $1)`, app)
	}
	for _, k := range v.SigningKeys {
		if k.AppID == "" { // the server's internal key keeps its (encrypted) private key
			exec(`INSERT INTO signing_keys (id, project_id, public_key_pem, private_key_encrypted) VALUES ($1, 'prj_default', $2, 'sealed')`, k.Kid, k.PublicKeyPEM)
		} else {
			exec(`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem) VALUES ($1, 'prj_default', $2, $3)`, k.Kid, k.AppID, k.PublicKeyPEM)
		}
		if k.Revoked {
			exec(`UPDATE signing_keys SET revoked_at = now() WHERE id = $1`, k.Kid)
		}
	}
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, a := range v.Assets {
		exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', $2, 'processing', 'sto_default')`, a.ID, a.AppID)
	}
	for _, o := range v.Outputs {
		manifest := any(nil)
		if o.Status != "writing" {
			manifest = "{}"
		}
		exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ($1, 'prj_default', $2, $3, $4, $5)`, o.ID, o.AssetID, o.KeyVersion, o.Status, manifest)
		for name, body := range v.Files {
			if err := store.Put(ctx, "media/"+o.AssetID+"/"+o.ID+"/"+name, strings.NewReader(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, a := range v.Assets {
		if a.CurrentOutputID != "" {
			exec(`UPDATE assets SET current_output_id = $1, status = $2 WHERE id = $3`, a.CurrentOutputID, a.Status, a.ID)
		}
	}
	for _, p := range v.PlaybackIDs {
		exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ($1, 'prj_default', $2, $3)`, p.ID, p.AssetID, p.Policy)
		if p.Status == "revoked" {
			exec(`UPDATE playback_ids SET status = 'revoked', revoked_at = now() WHERE id = $1`, p.ID)
		}
	}

	secret, err := contentkey.FromHex(v.ContentKeyVersion, v.ContentKeySecretHex)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pool, store, secret)
	h.Now = func() time.Time { return time.Unix(v.Now, 0) }
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p := c.Path
			for name, tok := range v.Tokens {
				p = strings.ReplaceAll(p, "{"+name+"}", tok)
			}
			res, err := http.Get(srv.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != c.Status {
				t.Fatalf("status = %d, want %d (%s)", res.StatusCode, c.Status, body)
			}
			if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("missing nosniff/CORS: %v", res.Header)
			}
			if c.ContentType != "" && res.Header.Get("Content-Type") != c.ContentType {
				t.Errorf("Content-Type = %q, want %q", res.Header.Get("Content-Type"), c.ContentType)
			}
			if c.CacheControl != "" && res.Header.Get("Cache-Control") != c.CacheControl {
				t.Errorf("Cache-Control = %q, want %q", res.Header.Get("Cache-Control"), c.CacheControl)
			}
			if c.Body != "" && string(body) != c.Body {
				t.Errorf("body = %q, want %q", body, c.Body)
			}
			if c.BodyHex != "" && hex.EncodeToString(body) != c.BodyHex {
				t.Errorf("body = %x, want %s", body, c.BodyHex)
			}
		})
	}
}

func TestRewriteMasterHandlesCRLF(t *testing.T) {
	got := RewriteMaster("#EXTM3U\r\n#EXT-X-MEDIA:URI=\"audio/index.m3u8\"\r\n360p/index.m3u8\r\n", "out_1")
	want := "#EXTM3U\n#EXT-X-MEDIA:URI=\"out_1/audio/index.m3u8\"\nout_1/360p/index.m3u8\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
}
