package accounts

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
)

// iterations is OWASP's 2023 figure for PBKDF2-HMAC-SHA256. One owner
// account, behind a login limiter, doesn't justify a dependency for argon2.
const iterations = 600_000

// HashPassword returns "pbkdf2-sha256$iterations$salt$hash" (base64, no
// padding) for a new password.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt) // crypto/rand.Read never fails (Go 1.24+)
	return hashWith(password, salt, iterations)
}

func hashWith(password string, salt []byte, iter int) string {
	key, _ := pbkdf2.Key(sha256.New, password, salt, iter, 32) // errs only on FIPS-mode limits
	enc := base64.RawStdEncoding
	return "pbkdf2-sha256$" + strconv.Itoa(iter) + "$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key)
}

// checkPassword reports whether password matches hash, and whether the hash
// should be replaced because it uses fewer iterations than today's.
func checkPassword(hash, password string) (ok, rehash bool) {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, false
	}
	iter, err := strconv.Atoi(parts[1])
	salt, serr := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || serr != nil || iter < 1 || iter > 10_000_000 {
		return false, false
	}
	match := subtle.ConstantTimeCompare([]byte(hashWith(password, salt, iter)), []byte(hash)) == 1
	return match, match && iter < iterations
}
