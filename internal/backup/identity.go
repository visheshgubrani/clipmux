package backup

import (
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"

	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// infoIdentity names the derivation of the install's backup identity from the
// secrets key (D143). Changing it makes every earlier backup unreadable with
// the key alone.
const infoIdentity = "clipmux-backup-v1"

// identityFor is the age identity of one version of the secrets key: its
// X25519 scalar is HKDF-SHA256 of the key. A backup is encrypted to the
// current version's, so the .env that holds the secrets key is the whole
// recovery kit, and anyone with the age CLI can open a backup with
// `clipmux admin backup identity`.
func identityFor(b *secrets.Box, version uint32) (*age.X25519Identity, error) {
	scalar, ok := b.Derive(version, infoIdentity)
	if !ok {
		return nil, fmt.Errorf("backup: the secrets box holds no key of version %d", version)
	}
	return age.ParseX25519Identity(encodeIdentity(scalar))
}

// identities are what opens a backup: the current key's identity and those of
// the previous keys the box still holds, so a rotation doesn't strand the
// backups made before it.
func identities(b *secrets.Box) ([]age.Identity, error) {
	var out []age.Identity
	for _, v := range b.Versions() {
		id, err := identityFor(b, v)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// IdentityInfo is one of the age identities that open this install's backups.
type IdentityInfo struct {
	// Version is the secrets key version it comes from.
	Version   uint32
	Recipient string
	// Secret is the private identity, AGE-SECRET-KEY-1…. Never log it.
	Secret string
}

// Identities lists the identities for `clipmux admin backup identity`, the
// current key's first.
func Identities(b *secrets.Box) ([]IdentityInfo, error) {
	var out []IdentityInfo
	for _, v := range b.Versions() {
		id, err := identityFor(b, v)
		if err != nil {
			return nil, err
		}
		out = append(out, IdentityInfo{Version: v, Recipient: id.Recipient().String(), Secret: id.String()})
	}
	return out, nil
}

// recipients are who a new backup is encrypted to: the install's identity,
// and any recipients the owner added (CLIPMUX_BACKUP_RECIPIENTS), such as an
// age key kept offline.
func recipients(b *secrets.Box, extra []string) ([]age.Recipient, error) {
	id, err := identityFor(b, b.Version())
	if err != nil {
		return nil, err
	}
	out := []age.Recipient{id.Recipient()}
	for _, s := range extra {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("CLIPMUX_BACKUP_RECIPIENTS: %q is not an age recipient (age1…): %w", s, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// encodeIdentity writes an X25519 scalar as age prints an identity:
// AGE-SECRET-KEY-1 and bech32. age has no constructor from raw bytes, and its
// bech32 is internal, so the encoding is here; age's own parser checks it.
func encodeIdentity(scalar []byte) string {
	s, _ := bech32Encode("age-secret-key-", scalar) // only for data that isn't bytes
	return strings.ToUpper(s)
}

// bech32 (BIP-173) without its 90-character limit, as age uses it.
const bech32Alphabet = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range gen {
			if top>>i&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32Encode(hrp string, data []byte) (string, error) {
	var five []byte
	acc, bits := uint32(0), 0
	for _, b := range data {
		acc = acc<<8 | uint32(b)
		for bits += 8; bits >= 5; bits -= 5 {
			five = append(five, byte(acc>>(bits-5))&31)
		}
		acc &= 1<<bits - 1
	}
	if bits > 0 {
		five = append(five, byte(acc<<(5-bits))&31)
	}
	if hrp == "" || hrp != strings.ToLower(hrp) {
		return "", errors.New("bech32: the prefix must be lower case")
	}
	var values []byte
	for i := range len(hrp) {
		values = append(values, hrp[i]>>5)
	}
	values = append(values, 0)
	for i := range len(hrp) {
		values = append(values, hrp[i]&31)
	}
	values = append(values, five...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ 1
	var sb strings.Builder
	sb.WriteString(hrp + "1")
	for _, v := range five {
		sb.WriteByte(bech32Alphabet[v])
	}
	for i := range 6 {
		sb.WriteByte(bech32Alphabet[mod>>(5*(5-i))&31])
	}
	return sb.String(), nil
}
