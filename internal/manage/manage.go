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
	"github.com/visheshgubrani/clipmux/internal/analytics"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/chapters"
	"github.com/visheshgubrani/clipmux/internal/cloudflare"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/modal"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/setuptest"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/speechapi"
	"github.com/visheshgubrani/clipmux/internal/stores"
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

	// Tests runs setup tests; nil leaves their routes answering 404.
	Tests *setuptest.Runner
	// Cloudflare connects Worker delivery; nil leaves its routes 404.
	Cloudflare *cloudflare.Connector
	// Edge pulls the Worker's analytics with a read-only token (P7-T05).
	Edge *analytics.Edge
	// RunPod runs Connect RunPod; nil leaves its routes 404.
	RunPod *runpod.Connector
	// Modal runs Connect Modal; nil leaves its routes 404.
	Modal *modal.Connector
	// Speech connects a speech API and sets the speech mode (D075); nil
	// leaves its routes 404.
	Speech *speechapi.Service
	// Chapters is the chapters LLM's Connect (P5-T17).
	Chapters *chapters.Service
	// Life queues library moves; nil leaves their routes 404.
	Life *lifecycle.Service
	// Stores lists, adds and removes storage configs; nil leaves their
	// routes 404.
	Stores *stores.Set
	// Keys rotates the content key secret; nil leaves its route 404.
	Keys   *ContentKeys
	Now    func() time.Time
	Logger *slog.Logger

	// approvals are the browser approvals waiting on the owner (D109).
	approvals approvals
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
	mux.HandleFunc("POST /v1/executors/modal/connect", m.owned(m.connectModal))
	mux.HandleFunc("POST /v1/executors/runpod/authorize", m.owned(m.startRunPodApproval))
	mux.HandleFunc("POST /v1/executors/modal/authorize", m.owned(m.startModalApproval))
	mux.HandleFunc("GET /v1/executors/{kind}/authorize/{id}", m.owned(m.pollApproval))
	mux.HandleFunc("GET /v1/executors/modal/gpus", m.owned(m.modalGPUs))
	mux.HandleFunc("POST /v1/executors/runpod/gpus", m.owned(m.runPodGPUs))
	mux.HandleFunc("DELETE /v1/executors/{kind}", m.owned(m.disconnectExecutor))
	mux.HandleFunc("GET /v1/settings/speech", m.owned(m.getSpeech))
	mux.HandleFunc("PUT /v1/settings/speech", m.owned(m.setSpeechMode))
	mux.HandleFunc("POST /v1/settings/speech/connect", m.owned(m.connectSpeech))
	mux.HandleFunc("DELETE /v1/settings/speech/connect", m.owned(m.disconnectSpeech))
	mux.HandleFunc("GET /v1/settings/chapters", m.owned(m.getChapters))
	mux.HandleFunc("POST /v1/settings/chapters/connect", m.owned(m.connectChapters))
	mux.HandleFunc("DELETE /v1/settings/chapters/connect", m.owned(m.disconnectChapters))
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
	mux.HandleFunc("POST /v1/webhook-endpoints/{id}/secret", m.owned(m.rotateEndpointSecret))
	mux.HandleFunc("POST /v1/webhook-endpoints/{id}/secret/activate", m.owned(m.activateEndpointSecret))
	mux.HandleFunc("GET /v1/webhook-endpoints/{id}/deliveries", m.owned(m.listDeliveries))
	mux.HandleFunc("POST /v1/webhook-deliveries/{id}/redeliver", m.owned(m.redeliver))
	mux.HandleFunc("GET /v1/library", m.owned(m.library))
	mux.HandleFunc("GET /v1/library/counts", m.owned(m.libraryCounts))
	mux.HandleFunc("POST /v1/assets/{id}/preview-token", m.owned(m.previewToken))
	mux.HandleFunc("POST /v1/setup-test", m.owned(m.startSetupTest))
	mux.HandleFunc("GET /v1/setup-test/latest", m.owned(m.latestSetupTest))
	mux.HandleFunc("GET /v1/cloudflare", m.owned(m.cloudflareStatus))
	mux.HandleFunc("POST /v1/cloudflare/connect", m.owned(m.connectCloudflare))
	mux.HandleFunc("POST /v1/cloudflare/inspect", m.owned(m.inspectCloudflare))
	mux.HandleFunc("DELETE /v1/cloudflare", m.owned(m.disconnectCloudflare))
	mux.HandleFunc("GET /v1/cloudflare/analytics", m.owned(m.edgeAnalytics))
	mux.HandleFunc("POST /v1/cloudflare/analytics", m.owned(m.connectEdgeAnalytics))
	mux.HandleFunc("DELETE /v1/cloudflare/analytics", m.owned(m.disconnectEdgeAnalytics))
	mux.HandleFunc("POST /v1/content-keys/rotate", m.owned(m.rotateContentKey))
	mux.HandleFunc("GET /v1/jobs", m.owned(m.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", m.owned(m.getJob))
	mux.HandleFunc("POST /v1/agents/{id}/reencode", m.owned(m.reencodeAgentOutputs))
	mux.HandleFunc("GET /v1/storage", m.owned(m.listStorage))
	mux.HandleFunc("POST /v1/storage", m.owned(m.addStorage))
	mux.HandleFunc("POST /v1/storage/r2", m.owned(m.addR2Storage))
	mux.HandleFunc("PUT /v1/storage/{id}/credentials", m.owned(m.rotateStorageCredentials))
	mux.HandleFunc("POST /v1/storage/{id}/default", m.owned(m.setDefaultStorage))
	mux.HandleFunc("DELETE /v1/storage/{id}", m.owned(m.removeStorage))
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
	media, err := q.GetMediaTarget(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	off, err := q.CountAssetsElsewhere(r.Context(), db.CountAssetsElsewhereParams{ProjectID: o.ProjectID, Target: def.ID, MediaTarget: media})
	if err != nil {
		return err
	}
	p, err := q.MoveProgress(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"default_storage_id": def.ID, "default_kind": def.Kind, "media_storage_id": media,
		"uploads_elsewhere": off.Uploads, "media_elsewhere": off.Media,
		"active": p.Active, "moved": p.Moved, "failed": p.Failed,
	})
	return nil
}

// moveLibrary queues a move for every asset with a part away from where the
// settings put it: sources on the default store, encoded files on the
// media target (D117).
func (m *API) moveLibrary(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Life == nil {
		return errNotFound
	}
	n, err := m.Life.MoveLibrary(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"queued": n})
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

// inspectCloudflare shows what a pasted token reaches (accounts, active
// zones, R2 buckets, Workers Paid) so the dashboard offers choices instead
// of typed IDs (D109). It changes nothing, and the token isn't kept.
func (m *API) inspectCloudflare(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Cloudflare == nil {
		return errNotFound
	}
	var req struct {
		Token     string `json:"token"`
		AccountID string `json:"account_id"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	in, err := m.Cloudflare.Inspect(r.Context(), req.Token, req.AccountID)
	switch {
	case errors.Is(err, cloudflare.ErrInvalid):
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), cloudflare.ErrInvalid.Error()+": "))
	case errors.Is(err, cloudflare.ErrTokenRejected):
		return httpjson.Fail(http.StatusBadRequest, "token_rejected", strings.TrimPrefix(err.Error(), cloudflare.ErrTokenRejected.Error()+": "))
	case err != nil:
		return err
	}
	httpjson.Write(w, http.StatusOK, in)
	return nil
}

// disconnectCloudflare moves delivery back to the built-in handler.
func (m *API) disconnectCloudflare(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Cloudflare == nil {
		return errNotFound
	}
	res, err := m.Cloudflare.Disconnect(r.Context())
	switch {
	case errors.Is(err, cloudflare.ErrNotConnected):
		return httpjson.Fail(http.StatusNotFound, "not_connected", "Cloudflare isn't connected")
	case errors.Is(err, cloudflare.ErrMediaURLPinned):
		return httpjson.Fail(http.StatusConflict, "pinned", "CLIPMUX_MEDIA_URL names the Worker; change it to the public URL and restart first")
	case err != nil:
		return err
	}
	httpjson.Write(w, http.StatusOK, res)
	return nil
}

// edgeAnalytics reports the edge analytics connection and its last pull.
func (m *API) edgeAnalytics(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Edge == nil {
		return errNotFound
	}
	st, err := m.Edge.Status(r.Context())
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, st)
	return nil
}

// connectEdgeAnalytics keeps a read-only Cloudflare token (Account
// Analytics: Read) that serve pulls the Worker's counts with.
func (m *API) connectEdgeAnalytics(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Edge == nil {
		return errNotFound
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	switch err := m.Edge.Connect(r.Context(), req.Token); {
	case errors.Is(err, analytics.ErrNoCloudflare):
		return httpjson.Fail(http.StatusConflict, "not_connected", "connect Cloudflare delivery first")
	case errors.Is(err, analytics.ErrTokenRejected):
		return httpjson.Fail(http.StatusBadRequest, "token_rejected", "Cloudflare rejected the token; it needs Account Analytics: Read on the delivery account")
	case err != nil:
		return err
	}
	return m.edgeAnalytics(w, r, accounts.Owner{})
}

// disconnectEdgeAnalytics forgets the token; counts already pulled stay.
func (m *API) disconnectEdgeAnalytics(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Edge == nil {
		return errNotFound
	}
	if err := m.Edge.Disconnect(r.Context()); errors.Is(err, analytics.ErrEdgeNotConnected) {
		return httpjson.Fail(http.StatusNotFound, "not_connected", "edge analytics isn't connected")
	} else if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
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
			m.Logger.ErrorContext(r.Context(), "manage api", "route", r.Pattern, "err", err)
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
		m.Logger.ErrorContext(r.Context(), "manage api", "route", r.Pattern, "err", err)
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

// createAPIKey issues a key. While the public URL is loopback, presigned
// upload URLs and base_url point at the owner's machine, so the owner must
// confirm the key is for apps on this machine (local) (D108). Keys are not
// bound to the URL: the same key works once a public URL is set.
func (m *API) createAPIKey(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	app, err := m.app(r, o)
	if err != nil {
		return err
	}
	if app.IsBuiltin {
		return apikeys.ErrBuiltinApp
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
		Local  bool     `json:"local"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	if !body.Local && !m.urls.KeysAllowed() {
		return httpjson.Fail(http.StatusConflict, "public_url_required", "set a public URL reachable from outside this machine, or confirm the key is for apps on this machine (local)")
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		body.Name = "default"
	}
	if len([]rune(body.Name)) > 100 {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "a key name is at most 100 characters")
	}
	if len(body.Scopes) == 0 {
		body.Scopes = apikeys.DefaultScopes()
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
		// The breaker (D069): failures in a row, and until when the
		// endpoint is skipped.
		Failures      int32      `json:"consecutive_failures"`
		SkippedUntil  *time.Time `json:"skipped_until,omitempty"`
		PendingSecret *time.Time `json:"pending_secret_since,omitempty"`
	}
	out := make([]view, 0, len(rows))
	for _, e := range rows {
		out = append(out, view{
			ID: e.ID, URL: e.Url, Created: e.CreatedAt.Time.UTC(), Disabled: optTime(e.DisabledAt),
			Failures: e.ConsecutiveFailures, SkippedUntil: optTime(e.CircuitOpenUntil), PendingSecret: optTime(e.PendingSecretAt),
		})
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

// rotateEndpointSecret makes a new secret, shown once; deliveries stay
// signed with the old one until it is activated.
func (m *API) rotateEndpointSecret(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	secret, err := webhooks.RotateSecret(r.Context(), db.New(m.pool), m.box, o.ProjectID, r.PathValue("id"))
	if errors.Is(err, webhooks.ErrNoEndpoint) {
		return httpjson.Fail(http.StatusNotFound, "not_found", "no such enabled endpoint")
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"secret": secret, "active": false})
	return nil
}

func (m *API) activateEndpointSecret(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	err := webhooks.ActivateSecret(r.Context(), db.New(m.pool), m.box, o.ProjectID, r.PathValue("id"))
	switch {
	case errors.Is(err, webhooks.ErrNoEndpoint):
		return httpjson.Fail(http.StatusNotFound, "not_found", "no such endpoint")
	case errors.Is(err, webhooks.ErrNoPendingSecret):
		return httpjson.Fail(http.StatusConflict, "no_pending_secret", "create a new secret first")
	case err != nil:
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"active": true})
	return nil
}

// listDeliveries shows an endpoint's latest 100 deliveries.
func (m *API) listDeliveries(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	q := db.New(m.pool)
	if ep, err := q.GetWebhookEndpoint(r.Context(), r.PathValue("id")); err != nil || ep.ProjectID != o.ProjectID {
		return httpjson.Fail(http.StatusNotFound, "not_found", "no such endpoint")
	}
	rows, err := q.ListEndpointDeliveries(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	type view struct {
		ID         string     `json:"id"`
		EventID    string     `json:"event_id"`
		EventType  string     `json:"event_type"`
		Status     string     `json:"status"`
		Attempts   int32      `json:"attempts"`
		LastStatus *int32     `json:"last_status,omitempty"`
		LastError  string     `json:"last_error,omitempty"`
		Created    time.Time  `json:"created_at"`
		Finished   *time.Time `json:"finished_at,omitempty"`
		Next       *time.Time `json:"next_attempt_at,omitempty"`
	}
	out := make([]view, 0, len(rows))
	for _, d := range rows {
		v := view{ID: d.ID, EventID: d.EventID, EventType: d.EventType, Status: d.Status, Attempts: d.Attempts,
			LastError: d.LastError.String, Created: d.CreatedAt.Time.UTC(), Finished: optTime(d.FinishedAt), Next: optTime(d.NextAttemptAt)}
		if d.LastStatus.Valid {
			v.LastStatus = &d.LastStatus.Int32
		}
		out = append(out, v)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

// redeliver sends a delivery that gave up again, with a fresh window.
func (m *API) redeliver(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	now := time.Now()
	row, err := db.New(m.pool).RedeliverWebhook(r.Context(), db.RedeliverWebhookParams{
		ID: r.PathValue("id"), ProjectID: o.ProjectID,
		Now:      pgtype.Timestamptz{Time: now, Valid: true},
		GiveUpAt: pgtype.Timestamptz{Time: now.Add(webhooks.RedeliverWindow), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return httpjson.Fail(http.StatusConflict, "not_redeliverable", "only a delivery that gave up (failed) can be sent again")
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"id": row.ID, "endpoint_id": row.EndpointID, "status": "pending"})
	return nil
}

// --- library and preview ---

const maxPage = 100

// library lists every app's assets, newest first. Deleted assets are left
// out unless include_deleted=true (or status=deleted); q finds an exact
// asset ID, or a title or passthrough containing it (D099). Each item
// carries a poster URL the dashboard can show as it is.
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
	withDeleted := false
	if d := query.Get("include_deleted"); d != "" {
		var err error
		if withDeleted, err = strconv.ParseBool(d); err != nil {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", "include_deleted must be true or false")
		}
	}
	search := strings.TrimSpace(query.Get("q"))
	if len(search) > maxSearch {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "q is limited to 255 bytes")
	}
	q := db.New(m.pool)
	rows, err := q.ListLibrary(r.Context(), db.ListLibraryParams{
		ProjectID: o.ProjectID, AppID: optText(query.Get("app_id")), Status: optText(query.Get("status")),
		IncludeDeleted: withDeleted, Search: optText(search), Pattern: optText(likePattern(search)),
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
	views, err := assets.LoadMany(r.Context(), q, rows)
	if err != nil {
		return err
	}
	items := make([]libraryItem, len(views))
	var signer signingkeys.Signer
	signed := false
	for i, v := range views {
		items[i].View = v
		pid, public := posterID(v)
		switch {
		case pid == "":
		case public:
			u := v.BaseURL + "/p/" + pid + "/poster.jpg"
			items[i].PosterURL = &u
		default:
			if !signed {
				if signer, err = signingkeys.Internal(r.Context(), m.pool, m.box, o.ProjectID); err != nil {
					return err
				}
				signed = true
			}
			tok, _, err := m.preview(signer, pid, tokens.AudienceThumbnail)
			if err != nil {
				return err
			}
			u := v.BaseURL + "/v/" + tok + "/" + pid + "/poster.jpg"
			items[i].PosterURL = &u
		}
	}
	body := map[string]any{"data": items}
	if next != "" {
		body["next_cursor"] = next
	}
	httpjson.Write(w, http.StatusOK, body)
	return nil
}

// maxSearch bounds the library's q, the length of a passthrough.
const maxSearch = 255

// libraryItem is an asset in the library: its view, and where its poster
// is, null until it has a playable ID.
type libraryItem struct {
	assets.View
	PosterURL *string `json:"poster_url"`
}

// posterID picks the playback ID a poster is fetched through: a public one
// needs no token, so it wins; else the first playable signed one. A share
// link's own ID is never used, so revoking a link leaves the poster alone.
func posterID(v assets.View) (string, bool) {
	pick := ""
	for _, p := range v.PlaybackIDs {
		if !p.Playable || p.Share != nil {
			continue
		}
		if p.Policy == "public" {
			return p.ID, true
		}
		if pick == "" {
			pick = p.ID
		}
	}
	return pick, false
}

// likePattern matches s anywhere, with LIKE's wildcards in s taken
// literally; "" stays "" (no search).
func likePattern(s string) string {
	if s == "" {
		return ""
	}
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// libraryCounts counts the library's assets by status, for the filter chips.
func (m *API) libraryCounts(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	c, err := db.New(m.pool).CountLibrary(r.Context(), db.CountLibraryParams{ProjectID: o.ProjectID, AppID: optText(r.URL.Query().Get("app_id"))})
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]int64{"ready": c.Ready, "processing": c.Processing, "errored": c.Errored, "waiting_upload": c.WaitingUpload})
	return nil
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
	tok, exp, err := m.preview(signer, pid, tokens.AudiencePlayback)
	if err != nil {
		return err
	}
	base, err := q.GetMediaURL(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"playback_id": pid, "token": tok, "expires_at": exp,
		"url": base + "/v/" + tok + "/" + pid + "/master.m3u8",
	})
	return nil
}

// preview signs a PreviewLifetime token for one playback ID with the
// server's internal key: aud v plays it, aud t fetches only its poster.
func (m *API) preview(signer signingkeys.Signer, pid, aud string) (string, time.Time, error) {
	now := m.Now().Unix()
	exp := now + int64(PreviewLifetime/time.Second)
	tok, err := signer.Sign(tokens.Claims{Sub: pid, Aud: aud, Iat: now, Exp: exp})
	return tok, time.Unix(exp, 0).UTC(), err
}

// firstLive picks the asset's first active playback ID, preferring a signed
// one (a public one plays under a token URL too). Share links' IDs are left
// out (D104), so revoking a link never ends a preview.
func firstLive(ctx context.Context, q *db.Queries, assetID string) (string, error) {
	pids, err := q.ListPreviewPlaybackIDs(ctx, assetID)
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
