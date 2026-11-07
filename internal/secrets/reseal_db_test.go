package secrets

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

func TestEverySealedColumnIsResealed(t *testing.T) {
	pool := dbtest.New(t)
	rows, err := pool.Query(context.Background(), `
		SELECT table_name || '.' || column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND data_type = 'bytea'
		  AND (column_name LIKE '%\_encrypted' OR column_name LIKE '%\_sealed' OR column_name = 'secrets_check')
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var known []string
	for _, c := range Columns {
		known = append(known, c.Table+"."+c.Column)
	}
	n := 0
	for rows.Next() {
		var col string
		_ = rows.Scan(&col)
		n++
		if !slices.Contains(known, col) {
			t.Errorf("%s holds sealed values but isn't in secrets.Columns: rotating the secrets key would strand it", col)
		}
	}
	if n != len(Columns) {
		t.Errorf("the schema has %d sealed columns, secrets.Columns lists %d", n, len(Columns))
	}
}

func TestRotatingTheSecretsKey(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	q := db.New(pool)
	v1, _ := NewBox(1, bytes.Repeat([]byte{1}, 32))
	if err := verify(ctx, q, "prj_default", v1); err != nil { // seals the check value
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_endpoints (id, project_id, app_id, url, secret_encrypted) VALUES ('whe_1', 'prj_default', 'app_dashboard', 'https://x.example', $1)`,
		v1.Seal("webhook_endpoints", "whe_1", []byte("whsec_old"))); err != nil {
		t.Fatal(err)
	}
	v2only, _ := NewBox(2, bytes.Repeat([]byte{2}, 32))
	err := CheckVersions(ctx, pool, v2only)
	if !errors.Is(err, ErrWrongKey) || !strings.Contains(err.Error(), "projects.secrets_check") {
		t.Fatalf("a new key alone before reseal: %v, want a refusal naming the column", err)
	}
	both, _ := NewBox(2, bytes.Repeat([]byte{2}, 32))
	if err := both.AddPrevious("v1:" + strings.Repeat("01", 32)); err != nil {
		t.Fatal(err)
	}
	if err := both.AddPrevious("v2:" + strings.Repeat("03", 32)); err == nil {
		t.Fatal("a previous key with the current version must be refused")
	}
	if err := CheckVersions(ctx, pool, both); err != nil {
		t.Fatalf("with the previous key beside it: %v", err)
	}
	counts, err := Reseal(ctx, pool, both)
	if err != nil {
		t.Fatal(err)
	}
	if counts["projects.secrets_check"] != 1 || counts["webhook_endpoints.secret_encrypted"] != 1 {
		t.Fatalf("resealed %v", counts)
	}
	if again, _ := Reseal(ctx, pool, both); again["webhook_endpoints.secret_encrypted"] != 0 {
		t.Fatal("a second reseal changed rows")
	}
	if err := CheckVersions(ctx, pool, v2only); err != nil {
		t.Fatalf("after reseal the new key alone must do: %v", err)
	}
	var sealed []byte
	_ = pool.QueryRow(ctx, `SELECT secret_encrypted FROM webhook_endpoints WHERE id = 'whe_1'`).Scan(&sealed)
	if plain, err := v2only.Open("webhook_endpoints", "whe_1", sealed); err != nil || string(plain) != "whsec_old" {
		t.Fatalf("open after reseal: %q %v", plain, err)
	}
	if err := verify(ctx, q, "prj_default", v2only); err != nil {
		t.Fatalf("the check value after reseal: %v", err)
	}
}
