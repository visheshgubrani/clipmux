package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// The known-device cookie (D121) marks a browser the owner signed in from:
// an HMAC over the owner and when it was issued. It is worth one thing, a
// sign-in that skips the global failure limit, so failures from other
// addresses can't lock the owner out. The per-address limit still applies,
// and a password change or reset retires every cookie issued before it.
const (
	deviceCookieBase = "clipmux_device"
	deviceLifetime   = 365 * 24 * time.Hour
	// deviceTable is what the device key is sealed under (secrets.Columns).
	deviceTable = "projects_device"
)

// deviceKey returns the project's device key, creating it on first use.
// Without a box there is none, and no cookie is issued or believed.
func (s *Service) deviceKey(ctx context.Context) ([]byte, error) {
	if s.Box == nil {
		return nil, nil
	}
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if s.deviceKeyBytes != nil {
		return s.deviceKeyBytes, nil
	}
	q := db.New(s.pool)
	sealed, err := q.GetDeviceKey(ctx, s.projectID)
	if err != nil {
		return nil, fmt.Errorf("read device key: %w", err)
	}
	if sealed == nil {
		key := make([]byte, 32)
		_, _ = rand.Read(key) // never fails (crypto/rand)
		if err := q.SetDeviceKeyIfUnset(ctx, db.SetDeviceKeyIfUnsetParams{ID: s.projectID, Sealed: s.Box.Seal(deviceTable, s.projectID, key)}); err != nil {
			return nil, fmt.Errorf("store device key: %w", err)
		}
		// Another serve may have stored one first; read back what won.
		if sealed, err = q.GetDeviceKey(ctx, s.projectID); err != nil {
			return nil, fmt.Errorf("read device key: %w", err)
		}
	}
	key, err := s.Box.Open(deviceTable, s.projectID, sealed)
	if err != nil {
		return nil, fmt.Errorf("open device key: %w", err)
	}
	s.deviceKeyBytes = key
	return key, nil
}

func deviceMAC(key []byte, projectID, ownerID, issued string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("clipmux-device-v1\n" + projectID + "\n" + ownerID + "\n" + issued))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) deviceCookieName() string {
	if s.https() {
		return "__Host-" + deviceCookieBase
	}
	return deviceCookieBase
}

// setDeviceCookie marks this browser as the owner's for a year. Failing to
// is logged, not returned: the sign-in itself succeeded.
func (s *Service) setDeviceCookie(ctx context.Context, w http.ResponseWriter, ownerID string) {
	key, err := s.deviceKey(ctx)
	if err != nil {
		s.Logger.WarnContext(ctx, "known-device cookie not issued", "err", err)
		return
	}
	if key == nil {
		return
	}
	issued := strconv.FormatInt(s.now().UnixMicro(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: s.deviceCookieName(), Value: ownerID + "." + issued + "." + deviceMAC(key, s.projectID, ownerID, issued),
		Path: "/", MaxAge: int(deviceLifetime.Seconds()), Secure: s.https(), HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func deviceValue(r *http.Request) string {
	for _, name := range []string{"__Host-" + deviceCookieBase, deviceCookieBase} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// knownDevice reports whether value is a device cookie this project issued
// to its owner within the last year and after the password last changed.
// Only a value whose MAC holds costs a database read.
func (s *Service) knownDevice(ctx context.Context, value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	ownerID, issuedText, mac := parts[0], parts[1], parts[2]
	micros, err := strconv.ParseInt(issuedText, 10, 64)
	if err != nil {
		return false
	}
	issued := time.UnixMicro(micros)
	if now := s.now(); issued.After(now.Add(time.Minute)) || now.Sub(issued) > deviceLifetime {
		return false
	}
	key, err := s.deviceKey(ctx)
	if err != nil || key == nil {
		return false
	}
	if !hmac.Equal([]byte(mac), []byte(deviceMAC(key, s.projectID, ownerID, issuedText))) {
		return false
	}
	owner, err := db.New(s.pool).GetOwner(ctx, s.projectID)
	if err != nil || owner.ID != ownerID {
		return false
	}
	return !issued.Before(owner.PasswordChangedAt.Time)
}
