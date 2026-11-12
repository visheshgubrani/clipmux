package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

const goodPassword = "correct horse battery staple"

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *Service
	srv    *httptest.Server
	public string
	hashes atomic.Int32
}

func newRig(t *testing.T, publicURL string) *rig {
	t.Helper()
	pool := dbtest.New(t)
	r := &rig{t: t, pool: pool, public: publicURL}
	r.svc = New(pool, "prj_default", func() string { return r.public })
	real := r.svc.check
	r.svc.check = func(hash, password string) (bool, bool) {
		r.hashes.Add(1)
		return real(hash, password)
	}
	mux := http.NewServeMux()
	r.svc.Register(mux)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) do(method, path string, body any, cookie *http.Cookie, header map[string]string) *http.Response {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { res.Body.Close() })
	return res
}

func sessionCookie(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if strings.HasSuffix(c.Name, "clipmux_session") {
			return c
		}
	}
	return nil
}

func (r *rig) register() *http.Cookie {
	r.t.Helper()
	token, open, err := r.svc.SetupToken(context.Background())
	if err != nil || !open {
		r.t.Fatalf("setup token: %v, open %v", err, open)
	}
	res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": token, "email": "owner@example.com", "password": goodPassword}, nil, nil)
	if res.StatusCode != http.StatusCreated {
		r.t.Fatalf("register = %d", res.StatusCode)
	}
	return sessionCookie(res)
}

func TestRegistrationWithTheSetupTokenCreatesTheOwnerOnce(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	token, open, err := r.svc.SetupToken(context.Background())
	if err != nil || !open || token == "" {
		t.Fatalf("fresh install: token %q, open %v, %v", token, open, err)
	}
	if res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": "wrong", "email": "owner@example.com", "password": goodPassword}, nil, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong setup token = %d, want 403", res.StatusCode)
	}
	if res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": token, "email": "owner@example.com", "password": "short"}, nil, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a short password = %d, want 400", res.StatusCode)
	}
	cookie := r.register()
	if cookie == nil {
		t.Fatal("registration must sign the owner in")
	}
	if res := r.do("GET", "/v1/session", nil, cookie, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("session after registration = %d", res.StatusCode)
	}
	res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": token, "email": "attacker@example.com", "password": goodPassword}, nil, nil)
	var body struct{ Error struct{ Code string } }
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusConflict || body.Error.Code != "registration_closed" {
		t.Fatalf("second registration = %d %s, want 409 registration_closed", res.StatusCode, body.Error.Code)
	}
	if _, open, _ := r.svc.SetupToken(context.Background()); open {
		t.Fatal("the setup token is no longer offered once an owner exists")
	}
}

func TestConcurrentRegistrationsCreateExactlyOneOwner(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	token, _, _ := r.svc.SetupToken(context.Background())
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := range 8 {
		wg.Go(func() {
			if _, _, err := r.svc.CreateOwner(context.Background(), token, "owner"+string(rune('a'+i))+"@example.com", goodPassword); err == nil {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	var owners int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM owners`).Scan(&owners)
	if won.Load() != 1 || owners != 1 {
		t.Fatalf("%d registrations won and %d owners exist, want exactly one", won.Load(), owners)
	}
}

func TestSessionCookieIsHostOnlyHttpOnlyStrict(t *testing.T) {
	r := newRig(t, "https://video.example.com")
	c := r.register()
	if c.Name != "__Host-clipmux_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("https cookie = %+v; want __Host- prefixed, Secure, HttpOnly, SameSite=Strict, Path=/, no Domain", c)
	}
	r2 := newRig(t, "http://192.168.1.10:8080")
	if c := r2.register(); c.Name != "clipmux_session" || c.Secure || !c.HttpOnly {
		t.Fatalf("plain-http cookie = %+v; want clipmux_session without Secure", c)
	}
}

func TestCrossOriginCookieRequestIsRefused(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	c := r.register()
	if res := r.do("DELETE", "/v1/session", nil, c, map[string]string{"Sec-Fetch-Site": "cross-site"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site DELETE with the cookie = %d, want 403", res.StatusCode)
	}
	if res := r.do("GET", "/v1/session", nil, c, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("the refused request must not have ended the session: %d", res.StatusCode)
	}
	if res := r.do("DELETE", "/v1/session", nil, c, map[string]string{"Sec-Fetch-Site": "same-origin"}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("same-origin sign-out = %d, want 204", res.StatusCode)
	}
	if res := r.do("GET", "/v1/session", nil, c, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after sign-out = %d, want 401", res.StatusCode)
	}
}

func TestSixthFailedLoginIsThrottledBeforeHashing(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	r.register()
	r.hashes.Store(0)
	for i := range 5 {
		if res := r.do("POST", "/v1/session", map[string]string{"email": "owner@example.com", "password": "not the password"}, nil, nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failed login %d = %d, want 401", i+1, res.StatusCode)
		}
	}
	res := r.do("POST", "/v1/session", map[string]string{"email": "owner@example.com", "password": goodPassword}, nil, nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("sixth login = %d (Retry-After %q), want 429 even with the right password", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if n := r.hashes.Load(); n != 5 {
		t.Fatalf("passwords hashed = %d, want 5: a throttled login must cost no hashing", n)
	}
}

func TestSessionsEndWhenIdleAndOnPasswordReset(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	c := r.register()
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `UPDATE owner_sessions SET last_seen_at = now() - interval '8 days'`); err != nil {
		t.Fatal(err)
	}
	if res := r.do("GET", "/v1/session", nil, c, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a session idle for 8 days = %d, want 401", res.StatusCode)
	}
	res := r.do("POST", "/v1/session", map[string]string{"email": "OWNER@example.com", "password": goodPassword}, nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", res.StatusCode)
	}
	fresh := sessionCookie(res)
	if err := ResetPassword(ctx, r.pool, "prj_default", "a brand new long password"); err != nil {
		t.Fatal(err)
	}
	if res := r.do("GET", "/v1/session", nil, fresh, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after a password reset = %d, want every session ended", res.StatusCode)
	}
	if res := r.do("POST", "/v1/session", map[string]string{"email": "owner@example.com", "password": "a brand new long password"}, nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("login with the new password = %d", res.StatusCode)
	}
}

func TestTheSixthWrongSetupTokenIsThrottled(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	for i := range 5 {
		res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": "wrong", "email": "owner@example.com", "password": goodPassword}, nil, nil)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("wrong token %d = %d, want 403", i+1, res.StatusCode)
		}
	}
	token, _, err := r.svc.SetupToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": token, "email": "owner@example.com", "password": goodPassword}, nil, nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("sixth try = %d (Retry-After %q), want 429 even with the right token", res.StatusCode, res.Header.Get("Retry-After"))
	}
}

func TestTheSecondFactorAtSignIn(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	box, err := secrets.NewBox(1, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r.svc.Box = box
	now := time.Now().Truncate(30 * time.Second) // the setup token is fresh, the step exact
	r.svc.Now = func() time.Time { return now }
	cookie := r.register()
	decode := func(res *http.Response) map[string]any {
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if res := r.do("POST", "/v1/session/totp", map[string]string{"password": "wrong password!"}, cookie, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("start with a wrong password: %d", res.StatusCode)
	}
	res := r.do("POST", "/v1/session/totp", map[string]string{"password": goodPassword}, cookie, nil)
	start := decode(res)
	if res.StatusCode != http.StatusCreated || !strings.HasPrefix(start["otpauth_url"].(string), "otpauth://totp/ClipMux:owner@example.com?") {
		t.Fatalf("start: %d %v", res.StatusCode, start)
	}
	secret, err := b32.DecodeString(start["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	code := func() string { return totpCode(secret, now.Unix()/30) }
	res = r.do("POST", "/v1/session/totp/enable", map[string]string{"code": code()}, cookie, nil)
	enabled := decode(res)
	codes, _ := enabled["recovery_codes"].([]any)
	if res.StatusCode != http.StatusOK || len(codes) != 10 {
		t.Fatalf("enable: %d %v", res.StatusCode, enabled)
	}
	login := func(extra map[string]string) (int, string) {
		body := map[string]string{"email": "owner@example.com", "password": goodPassword}
		for k, v := range extra {
			body[k] = v
		}
		res := r.do("POST", "/v1/session", body, nil, nil)
		out := decode(res)
		e, _ := out["error"].(map[string]any)
		code, _ := e["code"].(string)
		return res.StatusCode, code
	}
	if status, c := login(nil); status != 401 || c != "totp_required" {
		t.Fatalf("no code: %d %s", status, c)
	}
	// The enrolment code was this step's, so it can't sign in again.
	if status, c := login(map[string]string{"code": code()}); status != 401 || c != "invalid_code" {
		t.Fatalf("a used code: %d %s", status, c)
	}
	now = now.Add(30 * time.Second)
	if status, c := login(map[string]string{"code": code()}); status != 200 {
		t.Fatalf("a fresh code: %d %s", status, c)
	}
	if status, _ := login(map[string]string{"code": code()}); status != 401 {
		t.Fatal("the same code worked twice")
	}
	if status, c := login(map[string]string{"recovery_code": codes[3].(string)}); status != 200 {
		t.Fatalf("a recovery code: %d %s", status, c)
	}
	if status, c := login(map[string]string{"recovery_code": codes[3].(string)}); status != 401 || c != "invalid_code" {
		t.Fatalf("a recovery code twice: %d %s", status, c)
	}
	var sessions int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM owner_sessions`).Scan(&sessions)
	if sessions != 3 { // the enrolling one, and the two sign-ins since
		t.Fatalf("sessions = %d", sessions)
	}
}

func TestASetupTokenWorksForADay(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	now := time.Now()
	r.svc.Now = func() time.Time { return now }
	old, _, _ := r.svc.SetupToken(context.Background())
	now = now.Add(SetupTokenLifetime + time.Minute)
	res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": old, "email": "owner@example.com", "password": goodPassword}, nil, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a day-old token: %d, want 403", res.StatusCode)
	}
	fresh, open, _ := r.svc.SetupToken(context.Background())
	if fresh == old || !open {
		t.Fatal("an expired token must be replaced")
	}
	if res := r.do("POST", "/v1/setup/owner", map[string]string{"setup_token": fresh, "email": "owner@example.com", "password": goodPassword}, nil, nil); res.StatusCode != http.StatusCreated {
		t.Fatalf("the new token: %d", res.StatusCode)
	}
}

// D099: the owner changes the password from a session, giving the current
// one again; every other session ends and this one stays.
func TestChangingThePasswordEndsOtherSessions(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	here := r.register()
	res := r.do("POST", "/v1/session", map[string]string{"email": "owner@example.com", "password": goodPassword}, nil, nil)
	other := sessionCookie(res)
	const next = "a brand new long password"
	if res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": goodPassword, "new_password": "short"}, here, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a short new password = %d, want 400", res.StatusCode)
	}
	if res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": "not the password", "new_password": next}, here, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong current password = %d, want 401", res.StatusCode)
	}
	if res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": goodPassword, "new_password": next}, nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", res.StatusCode)
	}
	if res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": goodPassword, "new_password": next}, here, map[string]string{"Sec-Fetch-Site": "cross-site"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site change = %d, want 403", res.StatusCode)
	}
	if res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": goodPassword, "new_password": next}, here, nil); res.StatusCode != http.StatusNoContent {
		t.Fatalf("change = %d, want 204", res.StatusCode)
	}
	if res := r.do("GET", "/v1/session", nil, here, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("this session after the change = %d, want it kept", res.StatusCode)
	}
	if res := r.do("GET", "/v1/session", nil, other, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another session after the change = %d, want it ended", res.StatusCode)
	}
	if res := r.do("POST", "/v1/session", map[string]string{"email": "owner@example.com", "password": goodPassword}, nil, nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old password = %d, want 401", res.StatusCode)
	}
	if res := r.do("POST", "/v1/session", map[string]string{"email": "owner@example.com", "password": next}, nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("the new password = %d", res.StatusCode)
	}
}

// Wrong current passwords count as failed sign-ins: the sixth is refused
// before any hashing.
func TestWrongCurrentPasswordsAreThrottled(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	here := r.register()
	for i := range 5 {
		if res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": "not the password", "new_password": "a brand new long password"}, here, nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong current password %d = %d, want 401", i+1, res.StatusCode)
		}
	}
	before := r.hashes.Load()
	res := r.do("PUT", "/v1/session/password", map[string]string{"current_password": goodPassword, "new_password": "a brand new long password"}, here, nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" || r.hashes.Load() != before {
		t.Fatalf("sixth try = %d (Retry-After %q, hashed %d more), want 429 before hashing", res.StatusCode, res.Header.Get("Retry-After"), r.hashes.Load()-before)
	}
}
