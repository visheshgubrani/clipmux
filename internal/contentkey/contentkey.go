// Package contentkey derives per-output AES-128 content keys
// (docs/architecture.md, "Content protection"). A key is the first 16 bytes
// of an HMAC-SHA256 over the route's asset ID and the output ID, keyed with
// the project's content key secret, so a token for one video can't fetch
// another's key and nothing needs a key table. Losing the secret makes every
// output unplayable: back it up.
package contentkey

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Secret is a versioned content key secret.
type Secret struct {
	Version int32
	key     []byte
}

// Derive returns the content key for one output of one asset.
func (s Secret) Derive(assetID, outputID string) [16]byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte("clipmux-content-key-v1\n" + assetID + "\n" + outputID))
	var k [16]byte
	copy(k[:], h.Sum(nil))
	return k
}

// KeyID is the packager key ID for an output. It is not secret.
func KeyID(outputID string) [16]byte {
	sum := sha256.Sum256([]byte("clipmux-key-id-v1\n" + outputID))
	var k [16]byte
	copy(k[:], sum[:])
	return k
}

const fileName = "content.key"

// LoadOrCreate reads dir/content.key ("v{version}:{hex}"), creating a new
// version-1 secret on first boot. P1-T10 moves it into Postgres, encrypted
// with a key held outside it.
func LoadOrCreate(dir string) (Secret, error) {
	path := filepath.Join(dir, fileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return Secret{}, err
		}
		key := make([]byte, 32)
		rand.Read(key)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			return LoadOrCreate(dir)
		}
		if err != nil {
			return Secret{}, fmt.Errorf("create content key secret: %w", err)
		}
		_, werr := f.WriteString("v1:" + hex.EncodeToString(key))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return Secret{}, fmt.Errorf("write content key secret: %w", werr)
		}
		return Secret{Version: 1, key: key}, nil
	}
	if err != nil {
		return Secret{}, fmt.Errorf("read content key secret: %w", err)
	}
	v, h, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
	version, verr := strconv.ParseInt(strings.TrimPrefix(v, "v"), 10, 32)
	key, herr := hex.DecodeString(h)
	if !ok || !strings.HasPrefix(v, "v") || verr != nil || version < 1 || herr != nil || len(key) < 32 {
		return Secret{}, errors.New("contentkey: content.key is corrupt")
	}
	return Secret{Version: int32(version), key: key}, nil
}

// FromHex builds a secret from its hex encoding (test vectors and restores).
func FromHex(version int32, h string) (Secret, error) {
	key, err := hex.DecodeString(h)
	if err != nil || len(key) < 32 || version < 1 {
		return Secret{}, errors.New("contentkey: a secret is at least 32 bytes of hex with a version of 1 or more")
	}
	return Secret{Version: version, key: key}, nil
}
