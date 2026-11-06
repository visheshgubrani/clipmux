// Package manage is the owner's management API, the part of /v1 the
// dashboard uses beyond the integration API: the first-boot URL step, apps
// and their API keys, signing keys and webhook endpoints, the library across
// apps, and preview tokens. Every route needs the owner's session; no
// integration key can reach them (docs/architecture.md, "Accounts and
// tenancy"). The dashboard is a client of these public routes, not a
// private path.
package manage

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/cloudflare"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/setuptest"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

// PreviewLifetime is how long a dashboard preview token lasts; the player
// asks for a new one when it expires.
const PreviewLifetime = time.Hour

var errNotFound = httpjson.Fail(http.StatusNotFound, "not_found", "not found")

// API serves the management routes.
type API struct {
	pool     *pgxpool.Pool
	box      *secrets.Box
	accounts *accounts.Service
	urls     *settings.Store

	// URLsSaved runs after the owner saves new URLs (the setup test).
	URLsSaved func(settings.URLs)
	// Tests runs setup tests; nil leaves their routes answering 404.
	Tests *setuptest.Runner
	// Cloudflare connects Worker delivery; nil leaves its routes 404.
	Cloudflare *cloudflare.Connector
	// RunPod runs Connect RunPod; nil leaves its routes 404.
	RunPod *runpod.Connector
	// Life queues library moves; nil leaves their routes 404.
	Life   *lifecycle.Service
	Now    func() time.Time
	Logger *slog.Logger
}

// New returns the management API.
func New(pool *pgxpool.Pool, box *secrets.Box, acct *accounts.Service, urls *settings.Store) *API {
	return &API{pool: pool, box: box, accounts: acct, urls: urls, Now: time.Now, Logger: slog.New(slog.DiscardHandler)}
}

// ActAsApp lets the owner's session call the integration API as one app:
// the ClipMux-App header's, or the built-in Dashboard app. Serve installs it
// as the session resolver of every integration route.
func (m *API) ActAsApp(r *http.Request) (apikeys.Key, error) {
	owner, err := m.accounts.Owner(r)
	if err != nil {
		return apikeys.Key{}, err
	}
	q := db.New(m.pool)
	appID := r.Header.Get("ClipMux-App")
	if appID == "" {
		app, err := q.GetBuiltinApp(r.Context(), owner.ProjectID)
		if err != nil {
			return apikeys.Key{}, err
		}
		appID = app.ID
	} else if _, err := q.GetApp(r.Context(), db.GetAppParams{ProjectID: owner.ProjectID, ID: appID}); errors.Is(err, pgx.ErrNoRows) {
		return apikeys.Key{}, httpjson.Fail(http.StatusNotFound, "not_found", "no such app")
	} else if err != nil {
		return apikeys.Key{}, err
	}
	return apikeys.Key{ID: "owner:" + owner.ID, ProjectID: owner.ProjectID, AppID: appID, Scopes: apikeys.AllScopes(), Owner: true}, nil
}

// Register adds the management routes.
func (m *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/setup", m.setup)
	mux.HandleFunc("PUT /v1/settings/urls", m.owned(m.saveURLs))
	mux.HandleFunc("GET /v1/settings/routing", m.owned(m.getRouting))
	mux.HandleFunc("PUT /v1/settings/routing", m.owned(m.saveRouting))
	mux.HandleFunc("GET /v1/executors", m.owned(m.listExecutors))
	mux.HandleFunc("POST /v1/executors/runpod/connect", m.owned(m.connectRunPod))
	mux.HandleFunc("DELETE /v1/executors/{kind}", m.owned(m.disconnectExecutor))
	mux.HandleFunc("GET /v1/apps", m.owned(m.listApps))
	mux.HandleFunc("POST /v1/apps", m.owned(m.createApp))
	mux.HandleFunc("POST /v1/apps/{id}/disable", m.owned(m.setDisabled(true)))
	mux.HandleFunc("POST /v1/apps/{id}/enable", m.owned(m.setDisabled(false)))
	mux.HandleFunc("GET /v1/apps/{id}/api-keys", m.owned(m.listAPIKeys))
	mux.HandleFunc("POST /v1/apps/{id}/api-keys", m.owned(m.createAPIKey))
	mux.HandleFunc("DELETE /v1/api-keys/{id}", m.owned(m.revokeAPIKey))
	mux.HandleFunc("GET /v1/apps/{id}/signing-keys", m.owned(m.listSigningKeys))
	mux.HandleFunc("POST /v1/apps/{id}/signing-keys", m.owned(m.createSigningKey))
	mux.HandleFunc("DELETE /v1/signing-keys/{id}", m.owned(m.revokeSigningKey))
	mux.HandleFunc("GET /v1/apps/{id}/webhook-endpoints", m.owned(m.listEndpoints))
	mux.HandleFunc("POST /v1/apps/{id}/webhook-endpoints", m.owned(m.createEndpoint))
	mux.HandleFunc("DELETE /v1/webhook-endpoints/{id}", m.owned(m.disableEndpoint))
	mux.HandleFunc("GET /v1/library", m.owned(m.library))
	mux.HandleFunc("POST /v1/assets/{id}/preview-token", m.owned(m.previewToken))
	mux.HandleFunc("POST /v1/setup-test", m.owned(m.startSetupTest))
	mux.HandleFunc("GET /v1/setup-test/latest", m.owned(m.latestSetupTest))
	mux.HandleFunc("GET /v1/cloudflare", m.owned(m.cloudflareStatus))
	mux.HandleFunc("POST /v1/cloudflare/connect", m.owned(m.connectCloudflare))
	mux.HandleFunc("GET /v1/jobs", m.owned(m.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", m.owned(m.getJob))
	mux.HandleFunc("POST /v1/agents/{id}/reencode", m.owned(m.reencodeAgentOutputs))
	mux.HandleFunc("GET /v1/storage/moves", m.owned(m.moveProgress))
	mux.HandleFunc("POST /v1/storage/moves", m.owned(m.moveLibrary))
}

// moveProgress reports the library move: assets still off the default store
// (they still need this server to play) and the move jobs by state.
func (m *API) moveProgress(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Life == nil {
		return errNotFound
	}
	q := db.New(m.pool)
	def, err := q.GetDefaultStorage(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	off, err := q.CountAssetsOffStore(r.Context(), db.CountAssetsOffStoreParams{ProjectID: o.ProjectID, Target: def.ID})
	if err != nil {
		return err
	}
	p, err := q.MoveProgress(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"default_storage_id": def.ID, "default_kind": def.Kind, "not_on_default": off,
		"active": p.Active, "moved": p.Moved, "failed": p.Failed,
	})
	return nil
}

// moveLibrary queues a move to the default store for every asset elsewhere.
func (m *API) moveLibrary(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Life == nil {
		return errNotFound
	}
	def, err := db.New(m.pool).GetDefaultStorage(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	n, err := m.Life.MoveLibrary(r.Context(), o.ProjectID, def.ID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"queued": n, "target_storage_id": def.ID})
	return nil
}

// cloudflareStatus reports the Cloudflare connection, whether this build
// carries a newer Worker, and whether R2 still accepts the credentials.
func (m *API) cloudflareStatus(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Cloudflare == nil {
		return errNotFound
	}
	st, err := m.Cloudflare.Status(r.Context())
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, st)
	return nil
}

// connectCloudflare runs Connect Cloudflare with a token the owner pastes.
// The token is used for this request only; every step's result comes back,
// and a failed step can be fixed and connected again.
func (m *API) connectCloudflare(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Cloudflare == nil {
		return errNotFound
	}
	var req cloudflare.Request
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, err := m.Cloudflare.Connect(r.Context(), req)
	if errors.Is(err, cloudflare.ErrInvalid) {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), cloudflare.ErrInvalid.Error()+": "))
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, res)
	return nil
}

// startSetupTest queues a setup test (or returns the one already queued).
func (m *API) startSetupTest(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Tests == nil {
		return errNotFound
	}
	t, err := m.Tests.Queue(r.Context(), "manual")
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, t)
	return nil
}

func (m *API) latestSetupTest(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Tests == nil {
		return errNotFound
	}
	t, ok, err := m.Tests.Latest(r.Context())
	if err != nil {
		return err
	}
	if !ok {
		return errNotFound
	}
	httpjson.Write(w, http.StatusOK, t)
	return nil
}

type ownedHandler func(w http.ResponseWriter, r *http.Request, owner accounts.Owner) error

// owned requires the owner's session and writes a handler's error.
func (m *API) owned(h ownedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		owner, err := m.accounts.Owner(r)
		if err == nil {
			err = h(w, r, owner)
		}
		if err != nil && !errors.Is(err, errWritten) && !httpjson.WriteProblem(w, err) {
			m.Logger.Error("manage api", "path", r.URL.Path, "err", err)
			httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		}
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !httpjson.Decode(w, r, v) {
		return errWritten
	}
	return nil
}

// errWritten means the response was already written (a body decode error).
var errWritten = errors.New("manage: response already written")

// --- setup and settings ---

// setup tells the first-boot wizard what it still needs. It needs no
// session: before registration there is none.
func (m *API) setup(w http.ResponseWriter, r *http.Request) {
	exists, err := db.New(m.pool).OwnerExists(r.Context(), m.urlsProject())
	if err != nil {
		m.Logger.Error("manage api", "path", r.URL.Path, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	body := map[string]any{"owner_exists": exists, "urls": m.urls.URLs(), "keys_allowed": m.urls.KeysAllowed(), "setup_test": nil}
	if m.Tests != nil {
		if t, ok, err := m.Tests.Latest(r.Context()); err == nil && ok {
			body["setup_test"] = map[string]any{"id": t.ID, "status": t.Status} // details need the session
		}
	}
	httpjson.Write(w, http.StatusOK, body)
}

func (m *API) urlsProject() string { return db.DefaultProjectID }

func (m *API) saveURLs(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	var body struct {
		Public string `json:"public_url"`
		Media  string `json:"media_url"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	u, err := m.urls.Set(r.Context(), body.Public, body.Media)
	switch {
	case errors.Is(err, settings.ErrPinned):
		return httpjson.Fail(http.StatusConflict, "pinned", "CLIPMUX_PUBLIC_URL sets the URLs on this instance")
	case err != nil && r.Context().Err() == nil && !isDB(err):
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", err.Error())
	case err != nil:
		return err
	}
	m.Logger.Info("URLs saved", "public_url", u.Public, "media_url", u.Media)
	if m.URLsSaved != nil {
		m.URLsSaved(u)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"urls": u, "keys_allowed": m.urls.KeysAllowed()})
	return nil
}

// isDB reports whether err came from saving rather than from validation.
func isDB(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr)
}

// --- apps ---

type appView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Builtin  bool      `json:"builtin"`
	Disabled bool      `json:"disabled"`
	Created  time.Time `json:"created_at"`
}

func viewApp(a db.App) appView {
	return appView{ID: a.ID, Name: a.Name, Builtin: a.IsBuiltin, Disabled: a.DisabledAt.Valid, Created: a.CreatedAt.Time.UTC()}
}

func (m *API) listApps(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	apps, err := db.New(m.pool).ListApps(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	out := make([]appView, 0, len(apps))
	for _, a := range apps {
		out = append(out, viewApp(a))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func (m *API) createApp(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	if n := len([]rune(body.Name)); n < 1 || n > 100 {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "an app name is 1 to 100 characters")
	}
	app, err := db.New(m.pool).CreateApp(r.Context(), db.CreateAppParams{ID: ids.New(ids.App), ProjectID: o.ProjectID, Name: body.Name})
	if isUniqueViolation(err) {
		return httpjson.Fail(http.StatusConflict, "name_taken", "another app has this name")
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, viewApp(app))
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func (m *API) setDisabled(disabled bool) ownedHandler {
	return func(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
		var app db.App
		err := pgx.BeginFunc(r.Context(), m.pool, func(tx pgx.Tx) error {
			var err error
			q := db.New(tx)
			if app, err = q.SetAppDisabled(r.Context(), db.SetAppDisabledParams{ProjectID: o.ProjectID, ID: r.PathValue("id"), Disabled: disabled}); err != nil {
				return err
			}
			// Delivery refuses a disabled app's IDs; the Worker learns it when
			// their routes are unpublished (and republished on enable).
			return routes.QueueAppRefresh(r.Context(), q, o.ProjectID, app.ID)
		})
		q := db.New(m.pool)
		if errors.Is(err, pgx.ErrNoRows) {
			if a, gerr := q.GetApp(r.Context(), db.GetAppParams{ProjectID: o.ProjectID, ID: r.PathValue("id")}); gerr == nil && a.IsBuiltin {
				return httpjson.Fail(http.StatusConflict, "builtin_app", "the Dashboard app can't be disabled")
			}
			return errNotFound
		}
		if err != nil {
			return err
		}
		httpjson.Write(w, http.StatusOK, viewApp(app))
		return nil
	}
}

// app loads the path's app.
func (m *API) app(r *http.Request, o accounts.Owner) (db.App, error) {
	app, err := db.New(m.pool).GetApp(r.Context(), db.GetAppParams{ProjectID: o.ProjectID, ID: r.PathValue("id")})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.App{}, errNotFound
	}
	return app, err
}

// --- API keys ---

type keyView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Scopes   []string   `json:"scopes"`
	Created  time.Time  `json:"created_at"`
	LastUsed *time.Time `json:"last_used_at,omitempty"`
	Revoked  *time.Time `json:"revoked_at,omitempty"`
	// Key is the secret, present only in the response that created it.
	Key string `json:"key,omitempty"`
}

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

func (m *API) listAPIKeys(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	rows, err := db.New(m.pool).ListAPIKeys(r.Context(), app.ID)
	if err != nil {
		return err
	}
	out := make([]keyView, 0, len(rows))
	for _, k := range rows {
		out = append(out, keyView{ID: k.ID, Name: k.Name, Scopes: k.Scopes, Created: k.CreatedAt.Time.UTC(), LastUsed: optTime(k.LastUsedAt), Revoked: optTime(k.RevokedAt)})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

// createAPIKey issues a key, refused while the public URL is loopback:
// presigned upload URLs and base_url would point at the owner's machine.
func (m *API) createAPIKey(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	if !m.urls.KeysAllowed() {
		return httpjson.Fail(http.StatusConflict, "public_url_required", "set a public URL reachable from outside this machine before creating API keys")
	}
	if body.Name == "" {
		body.Name = "default"
	}
	if len(body.Scopes) == 0 {
		body.Scopes = []string{apikeys.UploadsWrite, apikeys.AssetsRead, apikeys.AssetsWrite}
	}
	for _, s := range body.Scopes {
		if !slices.Contains(apikeys.AllScopes(), s) {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", "unknown scope "+strconv.Quote(s))
		}
	}
	secret, k, err := apikeys.Create(r.Context(), m.pool, o.ProjectID, app.ID, body.Name, body.Scopes)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, keyView{ID: k.ID, Name: k.Name, Scopes: k.Scopes, Created: k.CreatedAt.Time.UTC(), Key: secret})
	return nil
}

func (m *API) revokeAPIKey(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	n, err := db.New(m.pool).RevokeAPIKey(r.Context(), db.RevokeAPIKeyParams{ProjectID: o.ProjectID, ID: r.PathValue("id")})
	return noContent(w, n, err)
}

func noContent(w http.ResponseWriter, n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return errNotFound
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// --- signing keys ---

func (m *API) listSigningKeys(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	rows, err := db.New(m.pool).ListSigningKeys(r.Context(), pgtype.Text{String: app.ID, Valid: true})
	if err != nil {
		return err
	}
	type view struct {
		Kid     string     `json:"kid"`
		Created time.Time  `json:"created_at"`
		Retired *time.Time `json:"retired_at,omitempty"`
		Revoked *time.Time `json:"revoked_at,omitempty"`
	}
	out := make([]view, 0, len(rows))
	for _, k := range rows {
		out = append(out, view{Kid: k.ID, Created: k.CreatedAt.Time.UTC(), Retired: optTime(k.RetiredAt), Revoked: optTime(k.RevokedAt)})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func (m *API) createSigningKey(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	kid, privatePEM, err := signingkeys.CreateForApp(r.Context(), m.pool, o.ProjectID, app.ID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, map[string]string{"kid": kid, "private_key_pem": privatePEM})
	return nil
}

func (m *API) revokeSigningKey(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	var n int64
	err := pgx.BeginFunc(r.Context(), m.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if n, err = q.RevokeSigningKey(r.Context(), db.RevokeSigningKeyParams{ProjectID: o.ProjectID, ID: r.PathValue("id")}); err != nil || n == 0 {
			return err
		}
		// Revocation reaches the Worker through _keys.json (60 s cache).
		return routes.QueueKeys(r.Context(), q, o.ProjectID)
	})
	return noContent(w, n, err)
}

// --- webhook endpoints ---

func (m *API) listEndpoints(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	rows, err := db.New(m.pool).ListWebhookEndpoints(r.Context(), app.ID)
	if err != nil {
		return err
	}
	type view struct {
		ID       string     `json:"id"`
		URL      string     `json:"url"`
		Created  time.Time  `json:"created_at"`
		Disabled *time.Time `json:"disabled_at,omitempty"`
	}
	out := make([]view, 0, len(rows))
	for _, e := range rows {
		out = append(out, view{ID: e.ID, URL: e.Url, Created: e.CreatedAt.Time.UTC(), Disabled: optTime(e.DisabledAt)})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func (m *API) createEndpoint(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	ep, secret, err := webhooks.CreateEndpoint(r.Context(), db.New(m.pool), m.box, o.ProjectID, app.ID, body.URL)
	if errors.Is(err, webhooks.ErrInvalidURL) {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"id": ep.ID, "url": ep.Url, "created_at": ep.CreatedAt.Time.UTC(), "secret": secret})
	return nil
}

func (m *API) disableEndpoint(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	n, err := db.New(m.pool).DisableWebhookEndpoint(r.Context(), db.DisableWebhookEndpointParams{ProjectID: o.ProjectID, ID: r.PathValue("id")})
	return noContent(w, n, err)
}

// --- library and preview ---

const maxPage = 100

// library lists every app's assets, newest first.
func (m *API) library(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	query := r.URL.Query()
	before, beforeID := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "~"
	if c := query.Get("cursor"); c != "" {
		var err error
		if before, beforeID, err = assets.DecodeCursor(c); err != nil {
			return httpjson.Fail(http.StatusBadRequest, "invalid_cursor", "cursor is not one this API returned")
		}
	}
	limit := 50
	if l := query.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxPage {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
		}
		limit = n
	}
	q := db.New(m.pool)
	rows, err := q.ListLibrary(r.Context(), db.ListLibraryParams{
		ProjectID: o.ProjectID, AppID: optText(query.Get("app_id")), Status: optText(query.Get("status")),
		BeforeTime: pgtype.Timestamptz{Time: before, Valid: true}, BeforeID: beforeID, MaxAssets: int32(limit + 1),
	})
	if err != nil {
		return err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = assets.EncodeCursor(rows[limit-1].CreatedAt.Time, rows[limit-1].ID)
	}
	return assets.WriteList(r.Context(), w, q, rows, next)
}

func optText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// previewToken signs a one-hour playback token with the server's internal
// key, so the owner can watch any app's asset in the dashboard.
func (m *API) previewToken(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	q := db.New(m.pool)
	asset, err := q.GetAssetInProject(r.Context(), db.GetAssetInProjectParams{ProjectID: o.ProjectID, ID: r.PathValue("id")})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	pid, err := firstLive(r.Context(), q, asset.ID)
	if err != nil {
		return err
	}
	signer, err := signingkeys.Internal(r.Context(), m.pool, m.box, o.ProjectID)
	if err != nil {
		return err
	}
	now := m.Now().Unix()
	exp := now + int64(PreviewLifetime/time.Second)
	tok, err := signer.Sign(tokens.Claims{Sub: pid, Aud: tokens.AudiencePlayback, Iat: now, Exp: exp})
	if err != nil {
		return err
	}
	base, err := q.GetMediaURL(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"playback_id": pid, "token": tok, "expires_at": time.Unix(exp, 0).UTC(),
		"url": base + "/v/" + tok + "/" + pid + "/master.m3u8",
	})
	return nil
}

// firstLive picks the asset's first active playback ID, preferring a signed
// one (a public one plays under a token URL too).
func firstLive(ctx context.Context, q *db.Queries, assetID string) (string, error) {
	pids, err := q.ListPlaybackIDs(ctx, assetID)
	if err != nil {
		return "", err
	}
	pick := ""
	for _, p := range pids {
		if p.Status != "active" {
			continue
		}
		if p.Policy == "signed" {
			return p.ID, nil
		}
		if pick == "" {
			pick = p.ID
		}
	}
	if pick == "" {
		return "", httpjson.Fail(http.StatusConflict, "no_playback_id", "the asset has no active playback ID; add one first")
	}
	return pick, nil
}
