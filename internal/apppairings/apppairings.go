// Package apppairings connects an app's backend to ClipMux: it issues the
// app's credentials (Issue), and runs the device flow that delivers them to
// `npx @clipmux/node init` without anyone copying a secret by hand.
//
// The flow mirrors agent pairing (internal/agents) and shares its code
// format and limits (internal/pairing):
//
//  1. The CLI starts a pairing and shows its code and the /connect link.
//  2. The owner opens the link, checks the code matches, picks or creates
//     the app and approves. Approval issues the credentials and seals them
//     with the secrets key.
//  3. The CLI's next poll opens them, returns them and clears them, so they
//     are handed over exactly once.
//
// Pairings expire after 10 minutes; a denied, expired or collected one never
// yields credentials.
package apppairings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/pairing"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
)

// Lifetime is how long a pairing's code works.
const Lifetime = 10 * time.Minute

// sealedTable is the table credentials are sealed for (secrets.Columns).
const sealedTable = "app_pairings"

// Service runs app pairings and the owner's credentials route.
type Service struct {
	pool      *pgxpool.Pool
	box       *secrets.Box
	accounts  *accounts.Service
	urls      *settings.Store
	projectID string
	Logger    *slog.Logger
	limits    *pairing.Limits
}

// New returns the service.
func New(pool *pgxpool.Pool, box *secrets.Box, acct *accounts.Service, urls *settings.Store, projectID string) *Service {
	return &Service{
		pool: pool, box: box, accounts: acct, urls: urls, projectID: projectID,
		Logger: slog.New(slog.DiscardHandler), limits: pairing.NewLimits(),
	}
}

func codeHash(code string) []byte { return pairing.Hash("clipmux-app-pairing-v1\n" + code) }

var (
	errNotFound = httpjson.Fail(http.StatusNotFound, "pairing_not_found", "no such pairing code, or it expired")
	errTooMany  = httpjson.Fail(http.StatusTooManyRequests, "too_many_pairings", "too many apps are waiting to be approved; approve or let them expire first")
)

// Description is what the CLI says about itself when it starts a pairing.
type Description struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	// Client is the CLI and its version, e.g. "@clipmux/node 0.1.0".
	Client string `json:"client"`
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

func (d Description) clean() Description {
	d = Description{Name: trim(d.Name, 100), Hostname: trim(d.Hostname, 253), Client: trim(d.Client, 100)}
	if d.Name == "" {
		d.Name = trim(d.Hostname, 100)
	}
	return d
}

// Request is a started pairing: the code to show the owner and the secret
// the CLI polls with.
type Request struct {
	Code      string    `json:"code"`
	PollToken string    `json:"poll_token"`
	VerifyURL string    `json:"verify_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Interval  int       `json:"interval_seconds"`
}

// Start opens a pairing, waiting for the owner.
func (s *Service) Start(ctx context.Context, d Description, ip netip.Addr) (Request, error) {
	d = d.clean()
	code, poll := pairing.NewCode(), pairing.NewPollToken()
	var p db.AppPairing
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		// Serialise the count against concurrent starts.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('app_pairings'))"); err != nil {
			return err
		}
		n, err := qs.CountLiveAppPairings(ctx, s.projectID)
		if err != nil {
			return err
		}
		if n >= pairing.MaxPending {
			return errTooMany
		}
		p, err = qs.CreateAppPairing(ctx, db.CreateAppPairingParams{
			ID: ids.New(ids.Pairing), ProjectID: s.projectID, CodeHash: codeHash(pairing.NormalizeCode(code)),
			PollTokenHash: pairing.Hash(poll), Name: d.Name, Hostname: d.Hostname, Client: d.Client,
			Ip: pairing.Inet(ip), TtlSeconds: Lifetime.Seconds(),
		})
		return err
	})
	if err != nil {
		return Request{}, err
	}
	return Request{
		Code: code, PollToken: poll, ExpiresAt: p.ExpiresAt.Time.UTC(), Interval: int(pairing.PollInterval / time.Second),
		VerifyURL: strings.TrimRight(s.urls.PublicURL(), "/") + "/connect?code=" + code,
	}, nil
}

// PollResult is a pairing's state as the CLI sees it.
type PollResult struct {
	Status      string       `json:"status"` // pending, approved, denied or expired
	Credentials *Credentials `json:"credentials,omitempty"`
}

// Poll reports a pairing. The first poll after approval returns the
// credentials and clears them; a later one finds no pairing.
func (s *Service) Poll(ctx context.Context, pollToken string) (PollResult, error) {
	if pollToken == "" {
		return PollResult{}, errNotFound
	}
	var res PollResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		p, err := qs.LockAppPairingByPollToken(ctx, pairing.Hash(pollToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case p.Status == "denied":
			res.Status = "denied"
		case p.Expired:
			// Approved but never collected: drop the credentials.
			res.Status = "expired"
			return qs.ExpireAppPairing(ctx, p.ID)
		case p.Status == "consumed":
			// The credentials were already handed over; never twice.
			return errNotFound
		case p.Status == "pending":
			res.Status = "pending"
		default:
			raw, err := s.box.Open(sealedTable, p.ID, p.CredentialsSealed)
			if err != nil {
				return fmt.Errorf("open pairing credentials: %w", err)
			}
			var c Credentials
			if err := json.Unmarshal(raw, &c); err != nil {
				return fmt.Errorf("read pairing credentials: %w", err)
			}
			if err := qs.ConsumeAppPairing(ctx, p.ID); err != nil {
				return err
			}
			res = PollResult{Status: "approved", Credentials: &c}
		}
		return nil
	})
	return res, err
}

// Approval is the owner's answer: an existing app or a new one, and
// optionally a webhook endpoint.
type Approval struct {
	AppID      string `json:"app_id"`
	NewAppName string `json:"new_app_name"`
	WebhookURL string `json:"webhook_url"`
	// Local confirms the credentials are for apps on this machine (D108).
	Local bool `json:"local"`
}

var errNameTaken = httpjson.Fail(http.StatusConflict, "name_taken", "another app has this name")

// Approve issues the app's credentials and seals them for the pairing's
// next poll. Approving a pairing already decided changes nothing.
func (s *Service) Approve(ctx context.Context, code string, a Approval) (db.AppPairing, error) {
	if (a.AppID == "") == (strings.TrimSpace(a.NewAppName) == "") {
		return db.AppPairing{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "give app_id or new_app_name, not both")
	}
	return s.decide(ctx, code, func(qs *db.Queries, tx pgx.Tx, p db.AppPairing) (db.AppPairing, error) {
		app, err := s.pickApp(ctx, tx, a)
		if err != nil {
			return db.AppPairing{}, err
		}
		c, err := Issue(ctx, tx, s.box, app, Options{
			PublicURL: s.urls.PublicURL(), Local: a.Local, WebhookURL: a.WebhookURL, KeyName: p.Name,
		})
		if err != nil {
			return db.AppPairing{}, err
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return db.AppPairing{}, err
		}
		return qs.ApproveAppPairing(ctx, db.ApproveAppPairingParams{
			ID: p.ID, AppID: pgtype.Text{String: app.ID, Valid: true}, CredentialsSealed: s.box.Seal(sealedTable, p.ID, raw),
		})
	})
}

// Deny refuses a pairing; its CLI stops polling.
func (s *Service) Deny(ctx context.Context, code string) (db.AppPairing, error) {
	return s.decide(ctx, code, func(qs *db.Queries, _ pgx.Tx, p db.AppPairing) (db.AppPairing, error) {
		p.Status = "denied"
		return p, qs.DenyAppPairing(ctx, p.ID)
	})
}

// decide runs fn on a pending pairing, or returns the pairing as it is.
func (s *Service) decide(ctx context.Context, code string, fn func(*db.Queries, pgx.Tx, db.AppPairing) (db.AppPairing, error)) (db.AppPairing, error) {
	n := pairing.NormalizeCode(code)
	if n == "" {
		return db.AppPairing{}, errNotFound
	}
	var out db.AppPairing
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		p, err := qs.LockLiveAppPairingByCode(ctx, db.LockLiveAppPairingByCodeParams{ProjectID: s.projectID, CodeHash: codeHash(n)})
		if errors.Is(err, pgx.ErrNoRows) {
			// Decided already, or expired: deciding twice is harmless.
			out, err = qs.GetAppPairingByCode(ctx, db.GetAppPairingByCodeParams{ProjectID: s.projectID, CodeHash: codeHash(n)})
			if errors.Is(err, pgx.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		if err != nil {
			return err
		}
		out, err = fn(qs, tx, p)
		return err
	})
	return out, err
}

// pickApp finds the approved app, or creates the new one.
func (s *Service) pickApp(ctx context.Context, tx pgx.Tx, a Approval) (db.App, error) {
	qs := db.New(tx)
	if a.AppID != "" {
		app, err := qs.GetApp(ctx, db.GetAppParams{ProjectID: s.projectID, ID: a.AppID})
		if errors.Is(err, pgx.ErrNoRows) {
			return db.App{}, httpjson.Fail(http.StatusNotFound, "not_found", "no such app")
		}
		return app, err
	}
	name := strings.TrimSpace(a.NewAppName)
	if len([]rune(name)) > 100 {
		return db.App{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "an app name is 1 to 100 characters")
	}
	// A savepoint, so a taken name is a clean refusal rather than an
	// aborted transaction.
	var app db.App
	err := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
		var err error
		app, err = db.New(sp).CreateApp(ctx, db.CreateAppParams{ID: ids.New(ids.App), ProjectID: s.projectID, Name: name})
		return err
	})
	if isUniqueViolation(err) {
		return db.App{}, errNameTaken
	}
	return app, err
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// View is a pairing as the owner sees it on /connect.
type View struct {
	Status    string    `json:"status"` // pending, approved, denied, consumed or expired
	Name      string    `json:"name"`
	Hostname  string    `json:"hostname"`
	Client    string    `json:"client"`
	IP        string    `json:"ip,omitempty"`
	AppID     string    `json:"app_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func view(p db.AppPairing, now time.Time) View {
	v := View{
		Status: p.Status, Name: p.Name, Hostname: p.Hostname, Client: p.Client,
		AppID: p.AppID.String, ExpiresAt: p.ExpiresAt.Time.UTC(),
	}
	if p.Ip != nil {
		v.IP = p.Ip.String()
	}
	if (p.Status == "pending" || p.Status == "approved") && !now.Before(p.ExpiresAt.Time) {
		v.Status = "expired"
	}
	return v
}

// Get finds a pairing by its code for the owner.
func (s *Service) Get(ctx context.Context, code string) (db.AppPairing, error) {
	n := pairing.NormalizeCode(code)
	if n == "" {
		return db.AppPairing{}, errNotFound
	}
	p, err := db.New(s.pool).GetAppPairingByCode(ctx, db.GetAppPairingByCodeParams{ProjectID: s.projectID, CodeHash: codeHash(n)})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AppPairing{}, errNotFound
	}
	return p, err
}

// IssueForApp issues credentials for an existing app, in one transaction.
func (s *Service) IssueForApp(ctx context.Context, appID string, o Options) (Credentials, error) {
	var c Credentials
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		app, err := db.New(tx).GetApp(ctx, db.GetAppParams{ProjectID: s.projectID, ID: appID})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpjson.Fail(http.StatusNotFound, "not_found", "not found")
		}
		if err != nil {
			return err
		}
		c, err = Issue(ctx, tx, s.box, app, o)
		return err
	})
	return c, err
}
