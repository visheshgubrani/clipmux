// Package auth owns the owner account, sessions and the first-boot setup
// token (docs/architecture.md, "Accounts and tenancy" and "First boot").
package auth

import (
	"crypto/rand"
	"encoding/base64"
)

// SetupTokenBytes is the entropy of a setup token.
const SetupTokenBytes = 24

// NewSetupToken returns a one-time token printed to the logs on first boot.
// The first visitor must present it to create the owner account.
func NewSetupToken() string {
	b := make([]byte, SetupTokenBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error.
	return base64.RawURLEncoding.EncodeToString(b)
}
