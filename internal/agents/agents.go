// Package agents registers PC agents: workers on the owner's own machines
// that claim jobs through the worker API (docs/architecture.md, "Compute").
//
// An agent gets its key by pairing, in either direction, and in both the
// owner has approved it before the key exists (D043):
//
//   - Dashboard first: the owner mints a code on the Workers page and runs
//     the command it shows on the PC, which exchanges the code for a key.
//   - Agent first: `clipmux worker --server URL` on the PC asks for a code,
//     opens the dashboard's connect page on it, and polls until the owner
//     approves. This is the path for the machine the owner is sitting at.
//
// Keys are shown to the agent once and stored as SHA-256, like API keys.
// Revoking an agent cuts off its key and fences its running attempts.
package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/queue"
)

const (
	// PairingLifetime is how long a code works.
	PairingLifetime = 15 * time.Minute
	// MaxPendingPairings bounds the agent-started pairings waiting for the
	// owner, because anyone who can reach the server can start one.
	MaxPendingPairings = 10
	// PollInterval is how often an agent should poll its pairing.
	PollInterval = 2 * time.Second

	keyPrefix = "cmxa_"
	// codeAlphabet has no look-alikes (no 0/O, 1/I/L), so a code read off a
	// screen types back correctly.
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	codeLength   = 8
)

// Service pairs, authenticates and manages agents.
type Service struct {
	pool      *pgxpool.Pool
	queue     *queue.Queue
	accounts  *accounts.Service
	projectID string
	// PublicURL is the dashboard's origin, for the connect link.
	PublicURL func() string
	// Life creates assets on local sources for the owner's push --local;
	// nil refuses attach.
	Life   *lifecycle.Service
	Logger *slog.Logger
}

// New returns the agent service.
func New(pool *pgxpool.Pool, q *queue.Queue, acct *accounts.Service, projectID string, publicURL func() string) *Service {
	return &Service{pool: pool, queue: q, accounts: acct, projectID: projectID, PublicURL: publicURL, Logger: slog.New(slog.DiscardHandler)}
}

// newCode returns a code such as KXFT-9M2Q (about 39 bits).
func newCode() string {
	b := make([]byte, codeLength)
	for i := range b {
		var r [1]byte
		for {
			_, _ = rand.Read(r[:]) // crypto/rand.Read never returns an error.
			if int(r[0]) < 256-256%len(codeAlphabet) {
				break
			}
		}
		b[i] = codeAlphabet[int(r[0])%len(codeAlphabet)]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// normalizeCode accepts a code as a person types it: any case, with or
// without the dash or spaces. It returns "" for anything that can't be one.
func normalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ':
		case strings.ContainsRune(codeAlphabet, r):
			b.WriteRune(r)
		default:
			return ""
		}
	}
	if b.Len() != codeLength {
		return ""
	}
	return b.String()
}

func hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func codeHash(code string) []byte { return hash("clipmux-pairing-v1\n" + code) }

// Description is what an agent says about itself when pairing.
type Description struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Platform string `json:"platform"`
}

func (d Description) clean() Description {
	trim := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			s = s[:n]
		}
		return s
	}
	d = Description{Name: trim(d.Name, 100), Hostname: trim(d.Hostname, 253), Platform: trim(d.Platform, 64)}
	if d.Name == "" {
		d.Name = d.Hostname
	}
	if d.Name == "" {
		d.Name = "PC agent"
	}
	return d
}

// Credentials are what a paired agent keeps: shown once, never stored here.
type Credentials struct {
	AgentID string `json:"agent_id"`
	Key     string `json:"key"`
	Name    string `json:"name"`
}

var (
	errNotFound = httpjson.Fail(http.StatusNotFound, "pairing_not_found", "no such pairing code, or it expired")
	errTooMany  = httpjson.Fail(http.StatusTooManyRequests, "too_many_pairings", "too many machines are waiting to be approved; approve or let them expire first")
)

func inet(a netip.Addr) *netip.Addr {
	if !a.IsValid() {
		return nil
	}
	a = a.Unmap()
	return &a
}

// mint creates the agent for a pairing and consumes the pairing, in tx.
func (s *Service) mint(ctx context.Context, qs *db.Queries, p db.AgentPairing, d Description, ip netip.Addr) (Credentials, error) {
	key := keyPrefix + rand.Text()
	agent, err := qs.CreateAgent(ctx, db.CreateAgentParams{
		ID: ids.New(ids.Agent), ProjectID: p.ProjectID, Name: d.Name, KeyHash: hash(key),
		Hostname: d.Hostname, Platform: d.Platform, LastIp: inet(ip),
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("create agent: %w", err)
	}
	if err := qs.ConsumePairing(ctx, db.ConsumePairingParams{ID: p.ID, AgentID: pgtype.Text{String: agent.ID, Valid: true}}); err != nil {
		return Credentials{}, fmt.Errorf("consume pairing: %w", err)
	}
	return Credentials{AgentID: agent.ID, Key: key, Name: agent.Name}, nil
}

// Invite mints a code the owner has already approved (dashboard first).
func (s *Service) Invite(ctx context.Context, name string) (string, db.AgentPairing, error) {
	code := newCode()
	p, err := db.New(s.pool).CreatePairing(ctx, db.CreatePairingParams{
		ID: ids.New(ids.Pairing), ProjectID: s.projectID, CodeHash: codeHash(normalizeCode(code)),
		Status: "approved", Name: strings.TrimSpace(name), TtlSeconds: PairingLifetime.Seconds(),
	})
	if err != nil {
		return "", db.AgentPairing{}, fmt.Errorf("create pairing: %w", err)
	}
	return code, p, nil
}

// Redeem exchanges an approved code for the agent's key.
func (s *Service) Redeem(ctx context.Context, code string, d Description, ip netip.Addr) (Credentials, error) {
	n := normalizeCode(code)
	if n == "" {
		return Credentials{}, errNotFound
	}
	d = d.clean()
	var creds Credentials
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		p, err := qs.LockLivePairingByCode(ctx, codeHash(n))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (p.Status != "approved" || p.PollTokenHash != nil)) {
			// A code an agent started is redeemed only through its poll
			// token, so reading it off the agent's screen isn't enough.
			return errNotFound
		}
		if err != nil {
			return err
		}
		if p.Name != "" {
			d.Name = p.Name
		}
		creds, err = s.mint(ctx, qs, p, d, ip)
		return err
	})
	return creds, err
}

// Request is an agent-started pairing: the code to show the owner and the
// secret the agent polls with.
type Request struct {
	ID        string    `json:"pairing_id"`
	Code      string    `json:"code"`
	PollToken string    `json:"poll_token"`
	VerifyURL string    `json:"verify_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Interval  int       `json:"interval_seconds"`
}

// Start opens an agent-started pairing, waiting for the owner.
func (s *Service) Start(ctx context.Context, d Description, ip netip.Addr) (Request, error) {
	d = d.clean()
	code, poll := newCode(), rand.Text()
	var p db.AgentPairing
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		// Serialise the count against concurrent starts.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('agent_pairings'))"); err != nil {
			return err
		}
		n, err := qs.CountLivePairingRequests(ctx, s.projectID)
		if err != nil {
			return err
		}
		if n >= MaxPendingPairings {
			return errTooMany
		}
		p, err = qs.CreatePairing(ctx, db.CreatePairingParams{
			ID: ids.New(ids.Pairing), ProjectID: s.projectID, CodeHash: codeHash(normalizeCode(code)),
			PollTokenHash: hash(poll), Status: "pending", Name: d.Name, Hostname: d.Hostname,
			Platform: d.Platform, Ip: inet(ip), TtlSeconds: PairingLifetime.Seconds(),
		})
		return err
	})
	if err != nil {
		return Request{}, err
	}
	return Request{
		ID: p.ID, Code: code, PollToken: poll, ExpiresAt: p.ExpiresAt.Time, Interval: int(PollInterval / time.Second),
		VerifyURL: strings.TrimRight(s.PublicURL(), "/") + "/workers/connect?code=" + code,
	}, nil
}

// PollResult is a pairing's state as its agent sees it.
type PollResult struct {
	Status      string       `json:"status"` // pending, approved, denied or expired
	Credentials *Credentials `json:"credentials,omitempty"`
}

// Poll reports an agent-started pairing; once the owner approves it, the
// first poll creates the agent and returns its key.
func (s *Service) Poll(ctx context.Context, pollToken string, ip netip.Addr) (PollResult, error) {
	var res PollResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		p, err := qs.LockPairingByPollToken(ctx, hash(pollToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case p.Status == "denied":
			res.Status = "denied"
		case p.Status == "consumed":
			// The key was already handed over; it is never shown twice.
			return errNotFound
		case p.Expired:
			res.Status = "expired"
		case p.Status == "pending":
			res.Status = "pending"
		default:
			pairing := db.AgentPairing{ID: p.ID, ProjectID: p.ProjectID}
			creds, err := s.mint(ctx, qs, pairing, Description{Name: p.Name, Hostname: p.Hostname, Platform: p.Platform}.clean(), ip)
			if err != nil {
				return err
			}
			res = PollResult{Status: "approved", Credentials: &creds}
		}
		return nil
	})
	return res, err
}

// Decide approves or denies an agent-started pairing for the owner.
func (s *Service) Decide(ctx context.Context, code string, approve bool) (db.AgentPairing, error) {
	n := normalizeCode(code)
	if n == "" {
		return db.AgentPairing{}, errNotFound
	}
	var p db.AgentPairing
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		var err error
		p, err = qs.LockLivePairingByCode(ctx, codeHash(n))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (p.ProjectID != s.projectID || p.PollTokenHash == nil)) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if p.Status != "pending" {
			return nil // deciding twice is harmless
		}
		p.Status = "denied"
		if approve {
			p.Status = "approved"
		}
		return qs.SetPairingStatus(ctx, db.SetPairingStatusParams{ID: p.ID, Status: p.Status})
	})
	return p, err
}

// AuthenticateWorker implements workerapi.WorkerAuth for agent keys. A
// revoked or pending agent's key is refused like an unknown one.
func (s *Service) AuthenticateWorker(ctx context.Context, key string) (queue.Worker, bool) {
	if !strings.HasPrefix(key, keyPrefix) {
		return queue.Worker{}, false
	}
	a, err := db.New(s.pool).GetAgentByKeyHash(ctx, hash(key))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.Logger.Error("agents", "op", "authenticate", "err", err)
		}
		return queue.Worker{}, false
	}
	if a.Status != "approved" || a.ProjectID != s.projectID {
		return queue.Worker{}, false
	}
	return queue.Worker{Executor: "agent", AgentID: a.ID, LocalOnly: a.Scope == "local"}, true
}

// ReportClaim records what an agent reported at claim: its capabilities,
// whether its tools are sandboxed, and that it was seen.
func (s *Service) ReportClaim(r *http.Request, agentID string, info json.RawMessage, sandboxed bool) {
	qs := db.New(s.pool)
	if err := qs.UpdateAgentReport(r.Context(), db.UpdateAgentReportParams{ID: agentID, Capabilities: info, Sandboxed: sandboxed}); err != nil {
		s.Logger.Error("agents", "op", "report claim", "err", err)
	}
	s.touch(r, agentID)
}

// ReportBeat records that an attempt's agent was seen.
func (s *Service) ReportBeat(r *http.Request, agentID string) { s.touch(r, agentID) }

func (s *Service) touch(r *http.Request, agentID string) {
	if err := db.New(s.pool).TouchAgent(r.Context(), db.TouchAgentParams{ID: agentID, Ip: inet(s.accounts.ClientIP(r))}); err != nil {
		s.Logger.Error("agents", "op", "touch", "err", err)
	}
}

// Revoke cuts off an agent's key and fences its attempts that could still
// write, returning how many it fenced.
func (s *Service) Revoke(ctx context.Context, agentID string) (db.Agent, int, error) {
	a, err := db.New(s.pool).RevokeAgent(ctx, db.RevokeAgentParams{ProjectID: s.projectID, ID: agentID})
	if err != nil {
		return db.Agent{}, 0, err
	}
	n, err := s.queue.FenceAgent(ctx, agentID)
	return a, n, err
}
