package assets

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// AppView is GET /v1/app: what a key's app needs to integrate. The SDK's
// doctor checks the key and the signing key against it, and fills in
// base_url and the longest token delivery accepts.
type AppView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	PublicURL string   `json:"public_url"`
	// BaseURL is the media origin playback URLs start with.
	BaseURL                 string `json:"base_url"`
	MaxTokenLifetimeSeconds int32  `json:"max_token_lifetime_seconds"`
	// UploadOrigins are where a browser sends upload parts: the server
	// itself (filesystem storage) and each bucket's public endpoint. With
	// base_url, they are what a page with the uploader and player must
	// allow in its Content-Security-Policy.
	UploadOrigins []string    `json:"upload_origins"`
	SigningKeys   []PublicKey `json:"signing_keys"`
}

// PublicKey is one of the app's signing keys that still verifies tokens.
type PublicKey struct {
	Kid          string     `json:"kid"`
	PublicKeyPEM string     `json:"public_key_pem"`
	CreatedAt    time.Time  `json:"created_at"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
}

// LoadApp builds the view of key's app.
func LoadApp(ctx context.Context, q *db.Queries, key apikeys.Key) (AppView, error) {
	app, err := q.GetApp(ctx, db.GetAppParams{ProjectID: key.ProjectID, ID: key.AppID})
	if err != nil {
		return AppView{}, err
	}
	s, err := q.GetIntegrationSettings(ctx, key.ProjectID)
	if err != nil {
		return AppView{}, err
	}
	keys, err := q.ListAppPublicKeys(ctx, pgtype.Text{String: app.ID, Valid: true})
	if err != nil {
		return AppView{}, err
	}
	v := AppView{
		ID: app.ID, Name: app.Name, Scopes: key.Scopes, PublicURL: s.PublicUrl, BaseURL: s.BaseUrl,
		MaxTokenLifetimeSeconds: s.MaxTokenLifetimeSeconds, UploadOrigins: []string{},
		SigningKeys: make([]PublicKey, 0, len(keys)),
	}
	if o := originOf(s.PublicUrl); o != "" {
		v.UploadOrigins = append(v.UploadOrigins, o)
	}
	for _, k := range keys {
		pk := PublicKey{Kid: k.ID, PublicKeyPEM: k.PublicKeyPem, CreatedAt: k.CreatedAt.Time.UTC()}
		if k.RetiredAt.Valid {
			t := k.RetiredAt.Time.UTC()
			pk.RetiredAt = &t
		}
		v.SigningKeys = append(v.SigningKeys, pk)
	}
	return v, nil
}

// getApp answers GET /v1/app for any valid key, whatever its scopes.
func (a *API) getApp(w http.ResponseWriter, r *http.Request) {
	key, ok := a.Auth.Require(w, r, apikeys.AnyScope)
	if !ok {
		return
	}
	v, err := LoadApp(r.Context(), db.New(a.pool), key)
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if a.UploadOrigins != nil {
		buckets, err := a.UploadOrigins(r.Context(), key.ProjectID)
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		for _, o := range buckets {
			if !slices.Contains(v.UploadOrigins, o) {
				v.UploadOrigins = append(v.UploadOrigins, o)
			}
		}
	}
	httpjson.Write(w, http.StatusOK, v)
}

// originOf is a URL's scheme and host, or "" if it has neither.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
