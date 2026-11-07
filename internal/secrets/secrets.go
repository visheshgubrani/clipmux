// Package secrets seals the secrets kept in Postgres (webhook endpoint
// secrets, adapter credentials, the internal signing key and the content key
// secret) with a key held outside it (docs/architecture.md, "Threats and
// controls"), so a copy of the database alone reveals none of them.
//
// A sealed value is 0x01 | key version (uint32, big-endian) | 12-byte nonce |
// AES-256-GCM ciphertext and tag. The associated data names the table and row
// the value belongs to, so a ciphertext copied to another row won't open.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// ErrNoKey means neither CLIPMUX_SECRETS_KEY nor {data_dir}/secrets.key
// holds a key.
var ErrNoKey = errors.New("secrets: no secrets key (set CLIPMUX_SECRETS_KEY or restore secrets.key)")

// ErrWrongKey means a value was sealed with a different key or key version.
var ErrWrongKey = errors.New("secrets: sealed with a different secrets key")

const (
	format   = 0x01
	fileName = "secrets.key"
	// checkTable and checkPlaintext make the value sealed into
	// projects.secrets_check on first boot.
	checkTable     = "projects"
	checkPlaintext = "clipmux-secrets-check-v1"
)

// Box seals and opens secrets with one version of the secrets key.
type Box struct {
	version uint32
	aead    cipher.AEAD
	// previous opens values sealed with an earlier key while they are
	// resealed (CLIPMUX_SECRETS_KEY_PREVIOUS, D071); Seal never uses it.
	previous map[uint32]cipher.AEAD
}

// NewBox returns a Box for a 32-byte key.
func NewBox(version uint32, key []byte) (*Box, error) {
	if version < 1 || len(key) != 32 {
		return nil, errors.New("secrets: a key is 32 bytes with a version of 1 or more")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{version: version, aead: aead}, nil
}

func associated(table, rowID string) []byte {
	return []byte("clipmux-secret-v1\n" + table + "\n" + rowID)
}

// Seal encrypts plaintext for one row of one table.
func (b *Box) Seal(table, rowID string, plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails (Go 1.24+)
	return b.seal(table, rowID, plaintext, nonce)
}

func (b *Box) seal(table, rowID string, plaintext, nonce []byte) []byte {
	out := make([]byte, 5, 5+len(nonce)+len(plaintext)+b.aead.Overhead())
	out[0] = format
	binary.BigEndian.PutUint32(out[1:5], b.version)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, associated(table, rowID))
}

// Open decrypts a value sealed for the same table and row.
func (b *Box) Open(table, rowID string, sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < 5+n+b.aead.Overhead() || sealed[0] != format {
		return nil, errors.New("secrets: not a sealed value")
	}
	aead := b.aead
	if v := binary.BigEndian.Uint32(sealed[1:5]); v != b.version {
		if aead = b.previous[v]; aead == nil {
			return nil, ErrWrongKey
		}
	}
	plain, err := aead.Open(nil, sealed[5:5+n], sealed[5+n:], associated(table, rowID))
	if err != nil {
		// GCM can't tell a wrong key from a moved or tampered value.
		return nil, ErrWrongKey
	}
	return plain, nil
}

// parse reads "v{version}:{64 hex}".
func parse(s string) (*Box, error) {
	v, h, ok := strings.Cut(strings.TrimSpace(s), ":")
	version, verr := strconv.ParseUint(strings.TrimPrefix(v, "v"), 10, 32)
	key, herr := hex.DecodeString(h)
	if !ok || !strings.HasPrefix(v, "v") || verr != nil || version < 1 || herr != nil || len(key) != 32 {
		return nil, errors.New(`secrets: a secrets key is "v1:" followed by 64 hex digits`)
	}
	return NewBox(uint32(version), key)
}

// Load returns the key from env (CLIPMUX_SECRETS_KEY) if set, otherwise
// from dir/secrets.key. It never creates one: only serve does, so an admin
// command run with the wrong data directory can't seal secrets under a key
// serve doesn't have.
func Load(dir, env string) (*Box, error) {
	if env != "" {
		b, err := parse(env)
		if err != nil {
			return nil, fmt.Errorf("CLIPMUX_SECRETS_KEY: %w", err)
		}
		return b, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, fmt.Errorf("read secrets key: %w", err)
	}
	b, err := parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fileName, err)
	}
	return b, nil
}

// create writes a new version-1 key to dir/secrets.key (mode 0600). It
// fails if the file exists.
func create(dir string) (*Box, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	f, err := os.OpenFile(filepath.Join(dir, fileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create secrets key: %w", err)
	}
	_, werr := f.WriteString("v1:" + hex.EncodeToString(key))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("write secrets key: %w", werr)
	}
	return NewBox(1, key)
}

// LoadOrCreate is serve's boot step. It loads the key, creating one only on
// a database that holds no sealed secrets yet, adds the previous key when
// one is given (a rotation in progress), then checks that the box opens the
// project's check value (sealing it on first boot).
func LoadOrCreate(ctx context.Context, q *db.Queries, projectID, dir, env, previous string) (*Box, error) {
	b, err := Load(dir, env)
	if errors.Is(err, ErrNoKey) {
		check, cerr := q.GetSecretsCheck(ctx, projectID)
		if cerr != nil {
			return nil, fmt.Errorf("read secrets check: %w", cerr)
		}
		if check != nil {
			return nil, fmt.Errorf("this database holds secrets sealed with a key that is gone: %w", ErrNoKey)
		}
		b, err = create(dir)
	}
	if err != nil {
		return nil, err
	}
	if previous != "" {
		if err := b.AddPrevious(previous); err != nil {
			return nil, err
		}
	}
	if err := verify(ctx, q, projectID, b); err != nil {
		return nil, err
	}
	return b, nil
}

func verify(ctx context.Context, q *db.Queries, projectID string, b *Box) error {
	check, err := q.GetSecretsCheck(ctx, projectID)
	if err != nil {
		return fmt.Errorf("read secrets check: %w", err)
	}
	if check == nil {
		if _, err := q.SetSecretsCheck(ctx, db.SetSecretsCheckParams{ID: projectID, SecretsCheck: b.Seal(checkTable, projectID, []byte(checkPlaintext))}); err != nil {
			return fmt.Errorf("write secrets check: %w", err)
		}
		// Another serve may have written first; read back what won.
		if check, err = q.GetSecretsCheck(ctx, projectID); err != nil {
			return fmt.Errorf("read secrets check: %w", err)
		}
	}
	plain, err := b.Open(checkTable, projectID, check)
	if err != nil {
		return fmt.Errorf("the secrets key doesn't open this database's secrets: %w", err)
	}
	if subtle.ConstantTimeCompare(plain, []byte(checkPlaintext)) != 1 {
		return ErrWrongKey
	}
	return nil
}

// Version is the version of the key Seal uses.
func (b *Box) Version() uint32 { return b.version }

// Opens reports whether the box holds the key for version v.
func (b *Box) Opens(v uint32) bool { return v == b.version || b.previous[v] != nil }

// AddPrevious lets the box open values sealed with an earlier key, given as
// CLIPMUX_SECRETS_KEY is ("v1:" and 64 hex digits). Its version must differ
// from the current key's.
func (b *Box) AddPrevious(key string) error {
	prev, err := parse(key)
	if err != nil {
		return fmt.Errorf("CLIPMUX_SECRETS_KEY_PREVIOUS: %w", err)
	}
	if prev.version == b.version {
		return fmt.Errorf("CLIPMUX_SECRETS_KEY_PREVIOUS has the current key's version (v%d): give the new key the next version", b.version)
	}
	if b.previous == nil {
		b.previous = map[uint32]cipher.AEAD{}
	}
	b.previous[prev.version] = prev.aead
	return nil
}
