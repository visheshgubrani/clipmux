package backup

import (
	"os"
	"strings"
	"testing"
)

func TestTheConnectionKeepsThePasswordOutOfPgDumpsArguments(t *testing.T) {
	c, err := libpqConn("postgres://clipmux:p%40ss:w@db.internal:6543/clipmux?sslmode=require&pool_max_conns=8&statement_timeout=5s&sslrootcert=/etc/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.URL, "p%40ss") || strings.Contains(c.URL, "p@ss") {
		t.Errorf("the password is in the URL pg_dump is given: %s", c.URL)
	}
	if want := "postgres://clipmux@db.internal:6543/clipmux?sslmode=require&sslrootcert=%2Fetc%2Fca.pem"; c.URL != want {
		t.Errorf("URL = %s, want %s (pgx's own settings dropped, libpq's kept)", c.URL, want)
	}
	if c.Password != "p@ss:w" {
		t.Errorf("password = %q", c.Password)
	}
	if got := pgpassLine("clipmux", `a:b\c`); got != `*:*:*:clipmux:a\:b\\c` {
		t.Errorf("pgpass line = %s, want : and \\ escaped", got)
	}
	if _, err := libpqConn("host=db password=x"); err == nil {
		t.Error("a keyword DATABASE_URL was accepted; backups need a postgres:// URL")
	}
}

func TestAMissingPgDumpSaysHowToGetOne(t *testing.T) {
	s := &Service{Config: Config{PGDump: "/nonexistent/pg_dump"}}
	err := s.runDump(t.Context(), os.Stderr, "postgres://u@h/d", "public", "")
	if err == nil || !strings.Contains(err.Error(), "CLIPMUX_PG_DUMP") {
		t.Fatalf("err = %v, want it to name CLIPMUX_PG_DUMP", err)
	}
}
