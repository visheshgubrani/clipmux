package secrets

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Column is one place a sealed value is kept, and how its associated data
// names it. Every bytea column named *_encrypted, *_sealed or
// secrets_check must be here, or rotating the secrets key would strand it;
// a test checks the schema against this list.
type Column struct {
	Table, Column string
	// ID is the SQL expression for the row ID sealed into the value, and
	// Sealed the table name it was sealed under.
	ID, Sealed string
}

// Columns are every sealed value ClipMux keeps.
var Columns = []Column{
	{"projects", "secrets_check", "id", "projects"},
	{"storage_configs", "secrets_encrypted", "id", "storage_configs"},
	{"executor_configs", "secrets_encrypted", "id", "executor_configs"},
	{"dispatches", "key_sealed", "id", "dispatches"},
	{"signing_keys", "private_key_encrypted", "id", "signing_keys"},
	{"webhook_endpoints", "secret_encrypted", "id", "webhook_endpoints"},
	{"webhook_endpoints", "pending_secret_encrypted", "id", "webhook_endpoints_pending"},
	{"content_key_secrets", "secret_encrypted", "project_id || '/v' || version", "content_key_secrets"},
	{"owners", "totp_secret_encrypted", "id", "owners"},
}

// CheckVersions refuses a box that can't open every sealed value: after a
// new secrets key, before reseal-secrets, serve needs the old one too. The
// error names the column, so the operator knows what would break.
func CheckVersions(ctx context.Context, pool *pgxpool.Pool, b *Box) error {
	for _, c := range Columns {
		rows, err := pool.Query(ctx, fmt.Sprintf(
			`SELECT DISTINCT substring(%s FROM 2 FOR 4) FROM %s WHERE %s IS NOT NULL AND length(%s) > 5`,
			c.Column, c.Table, c.Column, c.Column))
		if err != nil {
			return fmt.Errorf("read %s.%s: %w", c.Table, c.Column, err)
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return fmt.Errorf("read %s.%s: %w", c.Table, c.Column, err)
		}
		for _, v := range versions {
			if len(v) == 4 && !b.Opens(binary.BigEndian.Uint32(v)) {
				return fmt.Errorf("%s.%s holds secrets sealed with secrets key v%d, which neither CLIPMUX_SECRETS_KEY (v%d) nor CLIPMUX_SECRETS_KEY_PREVIOUS opens: %w",
					c.Table, c.Column, binary.BigEndian.Uint32(v), b.version, ErrWrongKey)
			}
		}
	}
	return nil
}

// Reseal seals every value not yet under the box's current key with it,
// one column per transaction, and returns how many it resealed per column.
// It is safe to run again, and while serve runs: a value sealed meanwhile
// is already current.
func Reseal(ctx context.Context, pool *pgxpool.Pool, b *Box) (map[string]int, error) {
	var current [4]byte
	binary.BigEndian.PutUint32(current[:], b.version)
	out := map[string]int{}
	for _, c := range Columns {
		name := c.Table + "." + c.Column
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, fmt.Sprintf(
				`SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND substring(%s FROM 2 FOR 4) <> $1 FOR UPDATE`,
				c.ID, c.Column, c.Table, c.Column, c.Column), current[:])
			if err != nil {
				return err
			}
			type row struct {
				id     string
				sealed []byte
			}
			found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
				var x row
				return x, r.Scan(&x.id, &x.sealed)
			})
			if err != nil {
				return err
			}
			for _, x := range found {
				plain, err := b.Open(c.Sealed, x.id, x.sealed)
				if err != nil {
					return fmt.Errorf("open %s %s: %w", name, x.id, err)
				}
				if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE %s = $2`, c.Table, c.Column, c.ID),
					b.Seal(c.Sealed, x.id, plain), x.id); err != nil {
					return err
				}
			}
			out[name] = len(found)
			return nil
		})
		if err != nil {
			return out, fmt.Errorf("reseal %s: %w", name, err)
		}
	}
	return out, nil
}
