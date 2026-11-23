package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// connInfo is a DATABASE_URL as pg_dump should get it.
type connInfo struct {
	// URL is the connection without its password or pgx-only settings.
	URL      string
	User     string
	Password string
}

// libpqKeys are the URL settings libpq reads; pgx-only ones such as
// pool_max_conns or statement_timeout would make pg_dump refuse the URL.
var libpqKeys = []string{"sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout", "host", "port"}

func libpqConn(databaseURL string) (connInfo, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return connInfo{}, errors.New("backups need DATABASE_URL as a postgres:// URL")
	}
	var c connInfo
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
		u.User = url.User(c.User)
	}
	q := url.Values{}
	for _, k := range libpqKeys {
		if v := u.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	c.URL = u.String()
	return c, nil
}

// pgpassLine is a .pgpass entry for any host, port and database.
func pgpassLine(user, password string) string {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	return "*:*:*:" + esc.Replace(user) + ":" + esc.Replace(password)
}

// tail keeps the last bytes written to it, for an error message.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

// runDump writes a custom-format dump of one schema to w, of the exported
// snapshot when there is one.
func (s *Service) runDump(ctx context.Context, w io.Writer, databaseURL, schema, snapshot string) error {
	if _, err := exec.LookPath(s.Config.PGDump); err != nil {
		return fmt.Errorf("%s not found: install the PostgreSQL 18 client tools, or point CLIPMUX_PG_DUMP at pg_dump", s.Config.PGDump)
	}
	args := []string{"--format=custom", "--schema=" + schema}
	if snapshot != "" {
		args = append(args, "--snapshot="+snapshot)
	}
	return runPG(ctx, s.Config.PGDump, databaseURL, args, nil, w)
}

// pgRestoreBin is the pg_restore that matches the configured pg_dump: the same
// directory, or the PATH's when pg_dump is found there.
func (c Config) pgRestoreBin() string {
	if dir := filepath.Dir(c.PGDump); dir != "." {
		return filepath.Join(dir, "pg_restore")
	}
	return "pg_restore"
}

// runPG runs a PostgreSQL client tool against databaseURL, or with no
// connection when it is empty. The password goes in a 0600 PGPASSFILE that
// lives only as long as the command, never in the environment or the
// arguments, and the child gets no other secret. It runs at idle priority, so
// a dump or a restore doesn't slow the encodes and requests it shares a
// machine with.
func runPG(ctx context.Context, bin, databaseURL string, args []string, stdin io.Reader, stdout io.Writer) error {
	name := filepath.Base(bin)
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s not found: install the PostgreSQL 18 client tools", name)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "PGAPPNAME=clipmux-backup", "PGCONNECT_TIMEOUT=15"}
	if databaseURL != "" {
		conn, err := libpqConn(databaseURL)
		if err != nil {
			return err
		}
		if conn.Password != "" {
			dir, err := os.MkdirTemp("", "clipmux-pgpass-")
			if err != nil {
				return fmt.Errorf("create the password file: %w", err)
			}
			defer os.RemoveAll(dir)
			file := filepath.Join(dir, "pgpass")
			if err := os.WriteFile(file, []byte(pgpassLine(conn.User, conn.Password)+"\n"), 0o600); err != nil {
				return fmt.Errorf("create the password file: %w", err)
			}
			env = append(env, "PGPASSFILE="+file)
		}
		args = append([]string{"--no-password", "--dbname=" + conn.URL}, args...)
	}
	if nice, err := exec.LookPath("nice"); err == nil {
		args = append([]string{"-n", "19", path}, args...)
		path = nice
	}
	var stderr tail
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w: %s", name, err, bytes.TrimSpace(stderr.b))
	}
	return nil
}
