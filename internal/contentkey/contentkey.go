// Package contentkey derives per-output AES-128 content keys
// (docs/architecture.md, "Content protection"). A key is the first 16 bytes
// of an HMAC-SHA256 over the route's asset ID and the output ID, keyed with
// the project's content key secret, so a token for one video can't fetch
// another's key and nothing needs a key table. The secret lives in Postgres,
// sealed with the secrets key; losing either makes every output unplayable,
// so back both up.
package contentkey

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
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

// sealedTable is the table the sealed secrets live in.
const sealedTable = "content_key_secrets"

func rowID(projectID string, version int32) string {
	return projectID + "/v" + strconv.Itoa(int(version))
}

// Ring holds every version of a project's content key secret that an output
// still uses. New outputs use the newest.
type Ring struct {
	current  Secret
	versions map[int32]Secret
}

// NewRing returns a ring of secrets; the highest version is current.
func NewRing(secrets ...Secret) Ring {
	r := Ring{versions: make(map[int32]Secret, len(secrets))}
	for _, s := range secrets {
		r.versions[s.Version] = s
		if s.Version > r.current.Version {
			r.current = s
		}
	}
	return r
}

// Current is the version new outputs are encrypted with.
func (r Ring) Current() Secret { return r.current }

// Get returns one version.
func (r Ring) Get(version int32) (Secret, bool) {
	s, ok := r.versions[version]
	return s, ok
}

// Load reads a project's content key secrets from Postgres, sealed with the
// secrets key. On first boot it imports dir/content.key, the file earlier
// versions kept, or else creates version 1; the file is removed once the
// database holds the same secret. It refuses to load when an output uses a
// version the ring lacks, because that output could never play again.
func Load(ctx context.Context, conn db.DBTX, box *secrets.Box, projectID, dir string) (Ring, error) {
	q := db.New(conn)
	file, err := readFile(dir)
	if err != nil {
		return Ring{}, err
	}
	rows, err := q.ListContentKeySecrets(ctx, projectID)
	if err != nil {
		return Ring{}, fmt.Errorf("read content key secrets: %w", err)
	}
	if len(rows) == 0 {
		first := file
		if first.key == nil {
			first = Secret{Version: 1, key: make([]byte, 32)}
			_, _ = rand.Read(first.key)
		}
		if _, err := q.InsertContentKeySecret(ctx, db.InsertContentKeySecretParams{
			ProjectID: projectID, Version: first.Version, SecretEncrypted: box.Seal(sealedTable, rowID(projectID, first.Version), first.key),
		}); err != nil {
			return Ring{}, fmt.Errorf("store content key secret: %w", err)
		}
		// Another serve may have stored one first; read back what won.
		if rows, err = q.ListContentKeySecrets(ctx, projectID); err != nil {
			return Ring{}, fmt.Errorf("read content key secrets: %w", err)
		}
	}
	all := make([]Secret, 0, len(rows))
	for _, row := range rows {
		key, err := box.Open(sealedTable, rowID(projectID, row.Version), row.SecretEncrypted)
		if err != nil {
			return Ring{}, fmt.Errorf("open content key secret v%d: %w", row.Version, err)
		}
		all = append(all, Secret{Version: row.Version, key: key})
	}
	ring := NewRing(all...)
	if file.key != nil {
		stored, ok := ring.Get(file.Version)
		if !ok || !hmac.Equal(stored.key, file.key) {
			return Ring{}, fmt.Errorf("contentkey: %s holds a secret the database doesn't; move it aside if the database is current", filepath.Join(dir, fileName))
		}
		if err := os.Remove(filepath.Join(dir, fileName)); err != nil {
			return Ring{}, fmt.Errorf("remove imported content.key: %w", err)
		}
	}
	used, err := q.ListOutputKeyVersions(ctx, projectID)
	if err != nil {
		return Ring{}, fmt.Errorf("list output key versions: %w", err)
	}
	for _, v := range used {
		if _, ok := ring.Get(v); !ok {
			return Ring{}, fmt.Errorf("contentkey: outputs use content key secret version %d, which the database doesn't hold; restore it before serving", v)
		}
	}
	return ring, nil
}

// readFile reads dir/content.key ("v{version}:{hex}") if it exists.
func readFile(dir string) (Secret, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Secret{}, nil
	}
	if err != nil {
		return Secret{}, fmt.Errorf("read content key secret: %w", err)
	}
	v, h, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")
	version, verr := strconv.ParseInt(strings.TrimPrefix(v, "v"), 10, 32)
	if !ok || !strings.HasPrefix(v, "v") || verr != nil {
		return Secret{}, errors.New("contentkey: content.key is corrupt")
	}
	s, err := FromHex(int32(version), h)
	if err != nil {
		return Secret{}, errors.New("contentkey: content.key is corrupt")
	}
	return s, nil
}

// FromHex builds a secret from its hex encoding (test vectors and restores).
func FromHex(version int32, h string) (Secret, error) {
	key, err := hex.DecodeString(h)
	if err != nil || len(key) < 32 || version < 1 {
		return Secret{}, errors.New("contentkey: a secret is at least 32 bytes of hex with a version of 1 or more")
	}
	return Secret{Version: version, key: key}, nil
}

// WorkerSecret is the ring as the Cloudflare Worker's CONTENT_KEYS secret:
// {"<version>": "<hex>"}. It is the content key secret itself, so it goes
// only to Cloudflare as a Worker secret, never to a log or a response.
func (r Ring) WorkerSecret() string {
	m := make(map[string]string, len(r.versions))
	for v, s := range r.versions {
		m[strconv.Itoa(int(v))] = hex.EncodeToString(s.key)
	}
	b, _ := json.Marshal(m) // strings only; can't fail
	return string(b)
}
