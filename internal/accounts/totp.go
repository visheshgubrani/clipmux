package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP is HMAC-SHA1, as every authenticator app expects
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ratelimit"
)

// The owner's second factor (D071): RFC 6238 time-based codes, six digits
// every 30 seconds, one step either side accepted for clock drift. The last
// step accepted is stored, so a code works once.
const (
	totpStep      = 30 * time.Second
	totpDigits    = 6
	totpSkew      = 1
	recoveryCodes = 10
	sealedOwners  = "owners"
)

var (
	errTOTPRequired = httpjson.Fail(http.StatusUnauthorized, "totp_required", "enter the code from your authenticator app, or a recovery code")
	errBadCode      = httpjson.Fail(http.StatusUnauthorized, "invalid_code", "that code is wrong or was already used")
	errNoBox        = errors.New("accounts: no secrets box to seal the second factor with")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpCode is the code for one time step.
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// matchStep finds the step within the skew whose code is code, or -1.
func matchStep(secret []byte, code string, now time.Time) int64 {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return -1
	}
	step := now.Unix() / int64(totpStep/time.Second)
	for d := -int64(totpSkew); d <= totpSkew; d++ {
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, step+d)), []byte(code)) == 1 {
			return step + d
		}
	}
	return -1
}

func recoveryHash(code string) []byte {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))))
	return sum[:]
}

// checkSecondFactor runs after the password matched: an owner with TOTP on
// must also give a code, or one unused recovery code. A wrong one counts as
// a failed sign-in.
func (s *Service) checkSecondFactor(ctx context.Context, q *db.Queries, ownerID, code, recovery string) error {
	row, err := q.GetOwnerTOTP(ctx, ownerID)
	if err != nil {
		return err
	}
	if !row.TotpEnabledAt.Valid {
		return nil
	}
	switch {
	case recovery != "":
		n, err := q.UseRecoveryCode(ctx, db.UseRecoveryCodeParams{OwnerID: ownerID, CodeHash: recoveryHash(recovery)})
		if err != nil {
			return err
		}
		if n == 0 {
			return errBadCode
		}
		return nil
	case code == "":
		return errTOTPRequired
	}
	if s.Box == nil {
		return errNoBox
	}
	secret, err := s.Box.Open(sealedOwners, ownerID, row.TotpSecretEncrypted)
	if err != nil {
		return err
	}
	step := matchStep(secret, code, s.now())
	if step < 0 {
		return errBadCode
	}
	n, err := q.AcceptTOTPStep(ctx, db.AcceptTOTPStepParams{ID: ownerID, Step: pgtype.Int8{Int64: step, Valid: true}})
	if err != nil {
		return err
	}
	if n == 0 {
		return errBadCode // replayed
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// --- enrolment, with the owner's session ---

// handleStartTOTP checks the password and makes a new secret, waiting for a
// code to prove the authenticator holds it. The secret is shown as text and
// as an otpauth:// link.
func (s *Service) handleStartTOTP(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !httpjson.Decode(w, r, &body) || !s.passwordOK(w, r, owner.ID, body.Password) {
		return
	}
	if s.Box == nil {
		s.fail(w, "totp", errNoBox)
		return
	}
	secret := make([]byte, 20)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails
	if err := db.New(s.pool).SetOwnerTOTPSecret(r.Context(), db.SetOwnerTOTPSecretParams{ID: owner.ID, TotpSecretEncrypted: s.Box.Seal(sealedOwners, owner.ID, secret)}); err != nil {
		s.fail(w, "totp", err)
		return
	}
	text := b32.EncodeToString(secret)
	link := url.URL{Scheme: "otpauth", Host: "totp", Path: "/ClipMux:" + owner.Email,
		RawQuery: url.Values{"secret": {text}, "issuer": {"ClipMux"}, "digits": {"6"}, "period": {"30"}}.Encode()}
	httpjson.Write(w, http.StatusCreated, map[string]string{"secret": text, "otpauth_url": link.String()})
}

// handleEnableTOTP turns the second factor on with a first code, returns
// ten recovery codes once, and ends every other session.
func (s *Service) handleEnableTOTP(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !httpjson.Decode(w, r, &body) {
		return
	}
	var codes []string
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetOwnerTOTP(r.Context(), owner.ID)
		if err != nil {
			return err
		}
		if row.TotpSecretEncrypted == nil || row.TotpEnabledAt.Valid || s.Box == nil {
			return httpjson.Fail(http.StatusConflict, "totp_not_started", "start enrolment with your password first")
		}
		secret, err := s.Box.Open(sealedOwners, owner.ID, row.TotpSecretEncrypted)
		if err != nil {
			return err
		}
		step := matchStep(secret, body.Code, s.now())
		if step < 0 {
			return errBadCode
		}
		if _, err := q.EnableOwnerTOTP(r.Context(), db.EnableOwnerTOTPParams{ID: owner.ID, TotpLastStep: pgtype.Int8{Int64: step, Valid: true}}); err != nil {
			return err
		}
		if err := q.DeleteRecoveryCodes(r.Context(), owner.ID); err != nil {
			return err
		}
		for range recoveryCodes {
			raw := rand.Text()[:12]
			code := raw[:4] + "-" + raw[4:8] + "-" + raw[8:]
			if err := q.InsertRecoveryCode(r.Context(), db.InsertRecoveryCodeParams{ProjectID: owner.ProjectID, OwnerID: owner.ID, CodeHash: recoveryHash(code)}); err != nil {
				return err
			}
			codes = append(codes, code)
		}
		token := s.sessionToken(r)
		return q.DeleteOtherOwnerSessions(r.Context(), db.DeleteOtherOwnerSessionsParams{OwnerID: owner.ID, TokenHash: hashToken(token)})
	})
	if err != nil {
		s.fail(w, "totp", err)
		return
	}
	s.Logger.InfoContext(r.Context(), "second factor turned on; other sessions ended", "owner", owner.ID)
	httpjson.Write(w, http.StatusOK, map[string]any{"enabled": true, "recovery_codes": codes})
}

// handleDisableTOTP turns the second factor off, with the password.
func (s *Service) handleDisableTOTP(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !httpjson.Decode(w, r, &body) || !s.passwordOK(w, r, owner.ID, body.Password) {
		return
	}
	q := db.New(s.pool)
	if _, err := q.DisableOwnerTOTP(r.Context(), owner.ProjectID); err != nil {
		s.fail(w, "totp", err)
		return
	}
	if err := q.DeleteRecoveryCodes(r.Context(), owner.ID); err != nil {
		s.fail(w, "totp", err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"enabled": false})
}

// ownerFor is the session's owner for an account change, refusing a
// cross-site request.
func (s *Service) ownerFor(w http.ResponseWriter, r *http.Request) (Owner, bool) {
	if s.crossOrigin(w, r) {
		return Owner{}, false
	}
	owner, err := s.Owner(r)
	if err != nil {
		s.fail(w, "session", err)
		return Owner{}, false
	}
	return owner, true
}

// passwordOK re-checks the owner's password before an account change,
// limited like a sign-in.
func (s *Service) passwordOK(w http.ResponseWriter, r *http.Request, ownerID, password string) bool {
	key := ratelimit.AddrKey(s.clientIP(r))
	if ok, wait := s.limiter.Allow(key); !ok {
		ratelimit.Refuse(w, wait)
		return false
	}
	row, err := db.New(s.pool).GetOwnerTOTP(r.Context(), ownerID)
	if err != nil {
		s.fail(w, "password", err)
		return false
	}
	if ok, _ := s.check(row.PasswordHash, password); !ok {
		s.limiter.Fail(key)
		s.fail(w, "password", errBadCredentials)
		return false
	}
	return true
}
