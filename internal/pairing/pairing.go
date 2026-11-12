// Package pairing holds what the two device flows share: PC agents pairing
// with the server (internal/agents) and apps pairing for their credentials
// (internal/apppairings). A client asks for a short code, shows it, and
// polls with a secret until the owner approves the code in the dashboard.
//
// It owns the code format, how codes and poll tokens are stored (SHA-256
// only), and the limits on the routes anyone can reach (D067).
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/ratelimit"
)

const (
	// MaxPending bounds the client-started pairings waiting for the owner,
	// because anyone who can reach the server can start one.
	MaxPending = 10
	// PollInterval is how often a client should poll its pairing.
	PollInterval = 2 * time.Second

	// codeAlphabet has no look-alikes (no 0/O, 1/I/L), so a code read off a
	// screen types back correctly.
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	codeLength   = 8
)

// NewCode returns a code such as KXFT-9M2Q (about 39 bits).
func NewCode() string {
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

// NormalizeCode accepts a code as a person types it: any case, with or
// without the dash or spaces. It returns "" for anything that can't be one.
func NormalizeCode(s string) string {
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

// NewPollToken returns the secret a client polls its pairing with.
func NewPollToken() string { return rand.Text() }

// Hash is how codes and poll tokens are stored.
func Hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Inet is a client address as it is stored: unmapped, or nil if unknown.
func Inet(a netip.Addr) *netip.Addr {
	if !a.IsValid() {
		return nil
	}
	a = a.Unmap()
	return &a
}

// Limits rate the public pairing routes per address (D067): 20 pairings an
// hour, and 2 polls a second (a client polls every 2 s).
type Limits struct {
	starts, polls *ratelimit.Buckets
}

// Per-address limits.
const (
	StartsPerHour = 20
	PollsPerSec   = 2
)

// NewLimits returns the shipped limits.
func NewLimits() *Limits {
	return &Limits{
		starts: ratelimit.NewBuckets(StartsPerHour/3600.0, StartsPerHour),
		polls:  ratelimit.NewBuckets(PollsPerSec, 2*PollsPerSec),
	}
}

// Start spends one of ip's pairings; refused, it has written the 429.
func (l *Limits) Start(w http.ResponseWriter, ip netip.Addr) bool {
	return take(w, l.starts, ip)
}

// Poll spends one of ip's polls; refused, it has written the 429.
func (l *Limits) Poll(w http.ResponseWriter, ip netip.Addr) bool {
	return take(w, l.polls, ip)
}

func take(w http.ResponseWriter, b *ratelimit.Buckets, ip netip.Addr) bool {
	ok, wait := b.Take(ratelimit.AddrKey(ip))
	if !ok {
		ratelimit.Refuse(w, wait)
	}
	return ok
}
