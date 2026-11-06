// Package accounts is the owner account (docs/architecture.md, "Accounts and
// tenancy"): the first visitor registers with the setup token printed in the
// server log, registration then closes, and the dashboard signs in with a
// session cookie. Integrations never use it; they use API keys.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
)

const (
	sessionLifetime = 30 * 24 * time.Hour
	sessionIdle     = 7 * 24 * time.Hour
	minPassword     = 12
	maxPassword     = 1024
)

var (
	errBadSetupToken      = httpjson.Fail(http.StatusForbidden, "invalid_setup_token", "the setup token is wrong; it is printed in the server log")
	errRegistrationClosed = httpjson.Fail(http.StatusConflict, "registration_closed", "this instance already has an owner; sign in instead")
	errWeakPassword       = httpjson.Fail(http.StatusBadRequest, "weak_password", fmt.Sprintf("a password is %d to %d characters", minPassword, maxPassword))
	errBadEmail           = httpjson.Fail(http.StatusBadRequest, "invalid_request", "email must be an address")
	errBadCredentials     = httpjson.Fail(http.StatusUnauthorized, "invalid_credentials", "wrong email or password")
	// ErrNoSession means the request carries no valid owner session.
	ErrNoSession = httpjson.Fail(http.StatusUnauthorized, "unauthorized", "sign in to the dashboard")
)

// Owner is the signed-in owner.
type Owner struct {
	ID, ProjectID, Email string
}

// Service registers the owner, signs them in and checks their sessions.
type Service struct {
	pool       *pgxpool.Pool
	projectID  string
	setupToken string
	publicURL  func() string
	limiter    *limiter
	cop        http.CrossOriginProtection
	check      func(hash, password string) (ok, rehash bool)

	// TrustedProxies are the addresses whose X-Forwarded-For is believed
	// when limiting logins (CLIPMUX_TRUSTED_PROXIES).
	TrustedProxies []netip.Prefix
	Logger         *slog.Logger
}

// New returns the service with a fresh setup token. publicURL decides the
// cookie: a Secure, __Host- prefixed one when the public URL is https.
func New(pool *pgxpool.Pool, projectID string, publicURL func() string) *Service {
	return &Service{
		pool: pool, projectID: projectID, setupToken: rand.Text(), publicURL: publicURL,
		limiter: newLimiter(time.Now), check: checkPassword, Logger: slog.New(slog.DiscardHandler),
	}
}

// SetupToken returns the token that creates the owner, and whether
// registration is still open. Print it only while it is.
func (s *Service) SetupToken(ctx context.Context) (string, bool, error) {
	exists, err := db.New(s.pool).OwnerExists(ctx, s.projectID)
	if err != nil {
		return "", false, err
	}
	return s.setupToken, !exists, nil
}

func validEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	return at > 0 && at < len(e)-1 && len(e) <= 320 && !strings.ContainsAny(e, " \t\r\n")
}

func validPassword(p string) bool {
	n := len([]rune(p))
	return n >= minPassword && len(p) <= maxPassword
}

// CreateOwner creates the owner with the setup token and returns a new
// session token. Registration closes at once: the database allows one owner
// per project.
func (s *Service) CreateOwner(ctx context.Context, setupToken, email, password string) (Owner, string, error) {
	if subtle.ConstantTimeCompare([]byte(setupToken), []byte(s.setupToken)) != 1 {
		return Owner{}, "", errBadSetupToken
	}
	switch {
	case !validEmail(email):
		return Owner{}, "", errBadEmail
	case !validPassword(password):
		return Owner{}, "", errWeakPassword
	}
	var owner Owner
	var token string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.CreateOwner(ctx, db.CreateOwnerParams{ID: ids.New(ids.Owner), ProjectID: s.projectID, Email: email, PasswordHash: HashPassword(password)})
		if errors.Is(err, pgx.ErrNoRows) {
			return errRegistrationClosed
		}
		if err != nil {
			return err
		}
		owner = Owner{ID: row.ID, ProjectID: row.ProjectID, Email: row.Email}
		token, err = startSession(ctx, q, owner)
		return err
	})
	return owner, token, err
}

// Login checks the owner's email and password and returns a new session
// token. Attempts are limited per address before anything is hashed.
func (s *Service) Login(ctx context.Context, ip netip.Addr, email, password string) (Owner, string, error) {
	if ok, wait := s.limiter.allow(ip); !ok {
		p := httpjson.Fail(http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins; try again later")
		p.Details = map[string]string{"retry_after": strconv.Itoa(int(math.Ceil(wait.Seconds())))}
		return Owner{}, "", p
	}
	q := db.New(s.pool)
	row, err := q.GetOwnerByEmail(ctx, db.GetOwnerByEmailParams{ProjectID: s.projectID, Email: email})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Owner{}, "", err
	}
	hash := row.PasswordHash
	if hash == "" {
		hash = hashWith("", make([]byte, 16), iterations) // same cost for an unknown email
	}
	ok, rehash := s.check(hash, password)
	if !ok || row.ID == "" {
		s.limiter.fail(ip)
		return Owner{}, "", errBadCredentials
	}
	s.limiter.reset(ip)
	if rehash {
		if err := q.SetOwnerPassword(ctx, db.SetOwnerPasswordParams{ID: row.ID, PasswordHash: HashPassword(password)}); err != nil {
			return Owner{}, "", err
		}
	}
	owner := Owner{ID: row.ID, ProjectID: row.ProjectID, Email: row.Email}
	token, err := startSession(ctx, q, owner)
	return owner, token, err
}

func startSession(ctx context.Context, q *db.Queries, o Owner) (string, error) {
	token := rand.Text()
	err := q.CreateOwnerSession(ctx, db.CreateOwnerSessionParams{
		TokenHash: hashToken(token), ProjectID: o.ProjectID, OwnerID: o.ID, LifetimeSeconds: sessionLifetime.Seconds(),
	})
	return token, err
}

func hashToken(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}

// ResetPassword sets a new owner password and ends every session, for
// `clipmux admin reset-password` on the host.
func ResetPassword(ctx context.Context, conn db.DBTX, projectID, password string) error {
	if !validPassword(password) {
		return fmt.Errorf("a password is %d to %d characters", minPassword, maxPassword)
	}
	q := db.New(conn)
	owner, err := q.GetOwner(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("there is no owner yet; register in the dashboard with the setup token")
	}
	if err != nil {
		return err
	}
	if err := q.SetOwnerPassword(ctx, db.SetOwnerPasswordParams{ID: owner.ID, PasswordHash: HashPassword(password)}); err != nil {
		return err
	}
	_, err = q.DeleteOwnerSessions(ctx, owner.ID)
	return err
}

// --- cookies and sessions ---

const cookieBase = "clipmux_session"

// https reports whether the public URL is https; the cookie is then Secure
// and __Host- prefixed (host-only, Path=/).
func (s *Service) https() bool {
	u, err := url.Parse(s.publicURL())
	return err == nil && u.Scheme == "https"
}

func (s *Service) cookieName() string {
	if s.https() {
		return "__Host-" + cookieBase
	}
	return cookieBase
}

func (s *Service) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: maxAge,
		Secure: s.https(), HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Service) sessionToken(r *http.Request) string {
	for _, name := range []string{"__Host-" + cookieBase, cookieBase} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// Owner returns the owner a request's session cookie signs in. A
// cross-origin request that would change state is refused.
func (s *Service) Owner(r *http.Request) (Owner, error) {
	token := s.sessionToken(r)
	if token == "" {
		return Owner{}, ErrNoSession
	}
	if err := s.cop.Check(r); err != nil {
		return Owner{}, httpjson.Fail(http.StatusForbidden, "cross_origin", "cross-origin requests can't use the dashboard session")
	}
	q := db.New(s.pool)
	row, err := q.GetOwnerSession(r.Context(), db.GetOwnerSessionParams{TokenHash: hashToken(token), IdleSeconds: sessionIdle.Seconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return Owner{}, ErrNoSession
	}
	if err != nil {
		return Owner{}, err
	}
	if row.Stale {
		_ = q.TouchOwnerSession(r.Context(), hashToken(token)) // best effort, at most once a minute
	}
	return Owner{ID: row.OwnerID, ProjectID: row.ProjectID, Email: row.Email}, nil
}

// ClientIP is the request's client address, as clientIP finds it.
func (s *Service) ClientIP(r *http.Request) netip.Addr { return s.clientIP(r) }

// clientIP is the address logins are limited by: the connection's, or the
// forwarding chain's first untrusted hop when the connection is a trusted
// proxy (Caddy).
func (s *Service) clientIP(r *http.Request) netip.Addr {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, _ := netip.ParseAddr(host)
	if !s.trusted(addr) {
		return addr
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !s.trusted(hop) {
			return hop
		}
		addr = hop
	}
	return addr
}

func (s *Service) trusted(a netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// --- HTTP ---

// Register adds the setup and session routes.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/setup/owner", s.handleCreateOwner)
	mux.HandleFunc("POST /v1/session", s.handleLogin)
	mux.HandleFunc("GET /v1/session", s.handleSession)
	mux.HandleFunc("DELETE /v1/session", s.handleLogout)
}

type credentials struct {
	SetupToken string `json:"setup_token"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

func (s *Service) fail(w http.ResponseWriter, op string, err error) {
	var p *httpjson.Problem
	if errors.As(err, &p) && p.Details["retry_after"] != "" {
		w.Header().Set("Retry-After", p.Details["retry_after"])
	}
	if !httpjson.WriteProblem(w, err) {
		s.Logger.Error("accounts", "op", op, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// crossOrigin refuses state-changing requests from other sites, so another
// page can't sign the owner in or out.
func (s *Service) crossOrigin(w http.ResponseWriter, r *http.Request) bool {
	if err := s.cop.Check(r); err != nil {
		httpjson.Error(w, http.StatusForbidden, "cross_origin", "cross-origin requests can't use the dashboard session")
		return true
	}
	return false
}

func (s *Service) handleCreateOwner(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if s.crossOrigin(w, r) || !httpjson.Decode(w, r, &c) {
		return
	}
	owner, token, err := s.CreateOwner(r.Context(), c.SetupToken, c.Email, c.Password)
	if err != nil {
		s.fail(w, "create_owner", err)
		return
	}
	s.Logger.Info("owner account created; registration is closed", "owner", owner.ID)
	s.setCookie(w, token, int(sessionLifetime.Seconds()))
	httpjson.Write(w, http.StatusCreated, map[string]string{"email": owner.Email})
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if s.crossOrigin(w, r) || !httpjson.Decode(w, r, &c) {
		return
	}
	owner, token, err := s.Login(r.Context(), s.clientIP(r), c.Email, c.Password)
	if err != nil {
		s.fail(w, "login", err)
		return
	}
	s.setCookie(w, token, int(sessionLifetime.Seconds()))
	httpjson.Write(w, http.StatusOK, map[string]string{"email": owner.Email})
}

func (s *Service) handleSession(w http.ResponseWriter, r *http.Request) {
	owner, err := s.Owner(r)
	if err != nil {
		s.fail(w, "session", err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]string{"email": owner.Email})
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Owner(r); err != nil {
		s.fail(w, "logout", err)
		return
	}
	if err := db.New(s.pool).DeleteOwnerSession(r.Context(), hashToken(s.sessionToken(r))); err != nil {
		s.fail(w, "logout", err)
		return
	}
	s.setCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
