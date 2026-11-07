package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/agents"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/retention"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/setuptest"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

const adminUsage = `Usage:
  clipmux admin create-key --app NAME [--name KEY_NAME] [--scopes uploads:write,assets:read] [--allow-loopback]
  clipmux admin create-signing-key --app NAME
  clipmux admin create-webhook --app NAME --url URL
  clipmux admin create-pairing [--app PC-NAME]
  clipmux admin reset-password < new-password.txt
  clipmux admin setup-test [--wait] [--timeout 20m]
  clipmux admin migrate status | up | down-to VERSION --yes
  clipmux admin prune [--dry-run] [--files]
  clipmux admin reseal-secrets
  clipmux admin rotate-signing-key --app NAME
  clipmux admin list-keys --app NAME
  clipmux admin revoke-key ID
  clipmux admin reset-totp
  clipmux admin outbox [--retry ID]

Host-side administration until the dashboard exists. Reads DATABASE_URL, and
for create-webhook CLIPMUX_DATA_DIR or CLIPMUX_SECRETS_KEY (the same key serve
uses). Each command creates the app if it does not exist and prints a secret
once: create-key an API key, create-signing-key the private key your backend
signs playback tokens with (only the public key is stored), create-webhook
the secret that signs the endpoint's deliveries.

create-key refuses while the public URL is loopback (localhost), because
signed upload URLs and base_url would point at this machine; set the public
URL at first boot or with CLIPMUX_PUBLIC_URL, or pass --allow-loopback for
local development.

reset-password reads the owner's new password from stdin (one line) and ends
every dashboard session.

setup-test asks the running serve to put the sample through the configured
path; with --wait it prints the checks and exits 1 unless they all pass.

migrate shows which migrations the database has (status), applies pending
ones (up; serve and every other admin command do this too), or rolls back
every migration above VERSION (down-to, which needs --yes and the binary
that applied them). Migrations hold a lock, so they never run twice at once.

prune deletes rows past their retention windows now, as serve does every
hour, and prints what went; --dry-run counts without deleting. --files also
deletes stored files past theirs (retired outputs, replaced attempts'
files, orphaned prefixes, the filesystem store's leftovers), reading
CLIPMUX_DATA_DIR and CLIPMUX_SECRETS_KEY as serve does. outbox lists
route publications, wakes and stops still pending after an hour, which are
retried forever; --retry ID makes one due at once.

reseal-secrets seals every stored secret with CLIPMUX_SECRETS_KEY. To rotate
the secrets key, give serve the new key as CLIPMUX_SECRETS_KEY (with the next
version, "v2:…") and the old one as CLIPMUX_SECRETS_KEY_PREVIOUS, run
reseal-secrets, then drop the previous key.

rotate-signing-key creates a new signing key for the app and retires its
others at the end of the longest token they may have signed, so viewers
mid-session finish; switch the app's backend to the new key meanwhile.
list-keys shows the app's API and signing keys; revoke-key ends one at once
(an API key cmx_… ID, or a signing key's kid).

reset-totp turns off the owner's second factor, for an owner who lost both
the authenticator and the recovery codes.
`

const project = db.DefaultProjectID

// admin runs host-side administration commands against the database.
func admin(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "migrate" {
		return migrateCmd(args[1:], getenv, stdout, stderr)
	}
	if len(args) > 0 && (args[0] == "prune" || args[0] == "outbox") {
		return retentionCmd(args[0], args[1:], getenv, stdout, stderr)
	}
	if len(args) > 0 && slices.Contains([]string{"reseal-secrets", "rotate-signing-key", "list-keys", "revoke-key", "reset-totp"}, args[0]) {
		return keysCmd(args[0], args[1:], getenv, stdout, stderr)
	}
	if len(args) == 0 || !slices.Contains([]string{"create-key", "create-signing-key", "create-webhook", "create-pairing", "reset-password", "setup-test"}, args[0]) {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "app name (created if missing)")
	name := fs.String("name", "default", "key name (create-key)")
	scopes := fs.String("scopes", apikeys.UploadsWrite+","+apikeys.AssetsRead, "comma-separated scopes (create-key)")
	endpoint := fs.String("url", "", "endpoint URL (create-webhook)")
	allowLoopback := fs.Bool("allow-loopback", false, "issue a key while the public URL is loopback (create-key, development only)")
	wait := fs.Bool("wait", false, "wait for the result (setup-test)")
	timeout := fs.Duration("timeout", 20*time.Minute, "how long to wait (setup-test)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if (*app == "" && cmd != "reset-password" && cmd != "setup-test" && cmd != "create-pairing") || (cmd == "create-webhook" && *endpoint == "") {
		fmt.Fprintf(stderr, "clipmux admin %s: --app is required (and --url for create-webhook)\n\n%s", cmd, adminUsage)
		return 2
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	switch cmd {
	case "create-key":
		if err = keysAllowed(ctx, pool, getenv, *allowLoopback); err == nil {
			err = createKey(ctx, pool, *app, *name, strings.Split(*scopes, ","), stdout)
		}
	case "create-signing-key":
		err = createSigningKey(ctx, pool, *app, stdout)
	case "reset-password":
		err = resetPassword(ctx, pool, stdin, stdout)
	case "create-pairing":
		err = createPairing(ctx, pool, *app, stdout)
	case "setup-test":
		err = setupTest(ctx, pool, *wait, *timeout, time.Second, stdout)
	case "create-webhook":
		var box *secrets.Box
		if box, err = loadBox(getenv); err == nil {
			err = createWebhook(ctx, pool, box, *app, *endpoint, stdout)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// keysAllowed applies the first-boot rule: no API key while the public URL
// (pinned by CLIPMUX_PUBLIC_URL, or saved at first boot) is loopback.
func keysAllowed(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, allowLoopback bool) error {
	public := getenv("CLIPMUX_PUBLIC_URL")
	if public == "" {
		urls, err := db.New(pool).GetProjectURLs(ctx, project)
		if err != nil {
			return err
		}
		public = urls.PublicUrl.String
	}
	if allowLoopback || settings.KeysAllowed(public) {
		return nil
	}
	if public == "" {
		public = config.DefaultPublicURL
	}
	return fmt.Errorf("the public URL is %s; API keys need one reachable from outside this machine. Set it at first boot or with CLIPMUX_PUBLIC_URL, or pass --allow-loopback for local development", public)
}

// appID finds an app by name, creating it if needed.
func appID(ctx context.Context, q *db.Queries, name string) (string, error) {
	app, err := q.GetAppByName(ctx, db.GetAppByNameParams{ProjectID: project, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		app, err = q.CreateApp(ctx, db.CreateAppParams{ID: ids.New(ids.App), ProjectID: project, Name: name})
	}
	return app.ID, err
}

// createKey finds or creates the app and issues an API key for it.
func createKey(ctx context.Context, pool *pgxpool.Pool, appName, keyName string, scopes []string, out io.Writer) error {
	var secret, app string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if app, err = appID(ctx, db.New(tx), appName); err != nil {
			return err
		}
		secret, _, err = apikeys.Create(ctx, tx, project, app, keyName, scopes)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app:    %s (%s)\nscopes: %s\nkey:    %s\n\nThe key is shown once. Store it in your app's backend.\n",
		appName, app, strings.Join(scopes, ","), secret)
	return nil
}

// createSigningKey generates an ES256 key pair for an app, stores only the
// public key and prints the private key once.
func createSigningKey(ctx context.Context, pool *pgxpool.Pool, appName string, out io.Writer) error {
	var app, kid, privatePEM string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if app, err = appID(ctx, db.New(tx), appName); err != nil {
			return err
		}
		kid, privatePEM, err = signingkeys.CreateForApp(ctx, tx, project, app)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app: %s (%s)\nkid: %s\n\n%s\nSign playback tokens with this private key (ES256, kid %s).\nIt is shown once and not stored by ClipMux.\n",
		appName, app, kid, privatePEM, kid)
	return nil
}

// setupTest queues a setup test for serve to run and, with wait, reports it.
func setupTest(ctx context.Context, pool *pgxpool.Pool, wait bool, timeout, poll time.Duration, out io.Writer) error {
	runner := setuptest.New(pool, nil, nil, nil, nil, nil, "")
	t, err := runner.Queue(ctx, "cli")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "setup test %s queued; serve runs it\n", t.ID)
	if !wait {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for t.Status == "queued" || t.Status == "running" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("setup test %s still %s after %s; is serve running?", t.ID, t.Status, timeout)
		case <-time.After(poll):
		}
		if t, err = runner.Get(ctx, t.ID); err != nil {
			return err
		}
	}
	for _, c := range t.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(out, "  %s %s %s\n", mark, c.Name, c.Detail)
	}
	if t.Status != "passed" {
		return fmt.Errorf("setup test %s failed", t.ID)
	}
	fmt.Fprintf(out, "setup test %s passed\n", t.ID)
	return nil
}

// resetPassword sets the owner's password from the first line of in and
// ends every dashboard session.
func resetPassword(ctx context.Context, pool *pgxpool.Pool, in io.Reader, out io.Writer) error {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := accounts.ResetPassword(ctx, pool, project, strings.TrimRight(line, "\r\n")); err != nil {
		return err
	}
	fmt.Fprintln(out, "The owner's password is changed and every dashboard session has ended.")
	return nil
}

// createWebhook finds or creates the app and registers an endpoint for its
// events, printing the signing secret once.
func createWebhook(ctx context.Context, pool *pgxpool.Pool, box *secrets.Box, appName, url string, out io.Writer) error {
	var ep db.WebhookEndpoint
	var secret string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		app, err := appID(ctx, q, appName)
		if err != nil {
			return err
		}
		ep, secret, err = webhooks.CreateEndpoint(ctx, q, box, project, app, url)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app:      %s (%s)\nendpoint: %s\nurl:      %s\nsecret:   %s\n\n"+
		"Verify X-Webhook-Signature (sha256= hex HMAC-SHA256 of \"{X-Webhook-Timestamp}.{raw body}\")\n"+
		"with this secret. It is shown once.\n", appName, ep.AppID, ep.ID, ep.Url, secret)
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// createPairing mints a one-time code to connect a PC agent, approved by
// whoever can run admin commands on the host: for a server with no
// browser at hand. --app, if given, names the PC.
func createPairing(ctx context.Context, pool *pgxpool.Pool, name string, stdout io.Writer) error {
	code, p, err := agents.New(pool, nil, nil, project, nil).Invite(ctx, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "code: %s\nexpires: %s\n\nOn the PC, run:\n  clipmux worker --server URL --pair %s\n", code, p.ExpiresAt.Time.Format(time.RFC3339), code)
	return nil
}

// migrateCmd runs clipmux admin migrate. Unlike the other commands it
// doesn't migrate first: status must show what is pending.
func migrateCmd(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := func() int { fmt.Fprint(stderr, adminUsage); return 2 }
	if len(args) == 0 {
		return usage()
	}
	fs := flag.NewFlagSet("migrate "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "confirm rolling migrations back (down-to)")
	var version int64
	rest := args[1:]
	if args[0] == "down-to" {
		if len(rest) == 0 {
			return usage()
		}
		v, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || v < 0 {
			fmt.Fprintf(stderr, "clipmux admin migrate down-to: %q is not a migration version\n", rest[0])
			return 2
		}
		version, rest = v, rest[1:]
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() > 0 {
		return usage()
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	defer pool.Close()
	switch args[0] {
	case "status":
		st, err := db.MigrationStatus(ctx, pool)
		if err != nil {
			fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
			return 1
		}
		pending := 0
		for _, m := range st {
			state := "applied " + m.AppliedAt.UTC().Format(time.RFC3339)
			if !m.Applied {
				state, pending = "pending", pending+1
			}
			fmt.Fprintf(stdout, "%5d  %-40s %s\n", m.Version, m.Name, state)
		}
		fmt.Fprintf(stdout, "%d pending\n", pending)
	case "up":
		err = db.Migrate(ctx, pool)
	case "down-to":
		if !*yes {
			fmt.Fprintf(stderr, "clipmux admin migrate down-to %d drops what later migrations added; pass --yes to do it\n", version)
			return 2
		}
		err = db.MigrateDownTo(ctx, pool, version)
	default:
		return usage()
	}
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	return 0
}

// retentionCmd runs clipmux admin prune and outbox.
func retentionCmd(cmd string, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "count what would go without deleting it (prune)")
	retry := fs.String("retry", "", "make this pending outbox row due now (outbox)")
	withFiles := fs.Bool("files", false, "also delete stored files past their windows (prune)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	r := retention.New(pool)
	switch {
	case cmd == "prune":
		rules, err := r.Prune(ctx, *dryRun)
		for _, rule := range rules {
			fmt.Fprintf(stdout, "%-20s %d\n", rule.Name, rule.Deleted)
		}
		if err != nil {
			fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
			return 1
		}
		if *dryRun {
			fmt.Fprintln(stdout, "(dry run: nothing was deleted)")
		} else if *withFiles {
			n, err := pruneFiles(ctx, pool, getenv)
			fmt.Fprintf(stdout, "%-20s %d\n", "files", n)
			if err != nil {
				fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
				return 1
			}
		}
	case *retry != "":
		ok, err := r.RetryNow(ctx, *retry)
		if err != nil {
			fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
			return 1
		}
		if !ok {
			fmt.Fprintf(stderr, "clipmux admin: no pending outbox row %s\n", *retry)
			return 1
		}
		fmt.Fprintf(stdout, "%s is due now\n", *retry)
	default:
		rows, err := r.Stuck(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
			return 1
		}
		for _, row := range rows {
			fmt.Fprintf(stdout, "%s  %-10s tries %-3d since %s  next %s  %s\n", row.ID, row.Kind, row.Attempts,
				row.CreatedAt.Time.UTC().Format(time.RFC3339), row.AvailableAt.Time.UTC().Format(time.RFC3339), row.LastError)
		}
		fmt.Fprintf(stdout, "%d stuck (pending for over %s)\n", len(rows), retention.StuckAfter)
	}
	return 0
}

// pruneFiles runs the file retention rules over the install's stores.
func pruneFiles(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string) (int, error) {
	dataDir := orDefault(getenv("CLIPMUX_DATA_DIR"), config.DefaultDataDir)
	box, err := loadBox(getenv)
	if err != nil {
		return 0, err
	}
	fsStore, err := storage.OpenFilesystem(filepath.Join(dataDir, "storage"), storage.FilesystemOptions{BaseURL: config.DefaultPublicURL})
	if err != nil {
		return 0, err
	}
	defer fsStore.Close()
	return retention.NewFiles(pool, stores.New(pool, box, fsStore), fsStore).Run(ctx)
}

// loadBox loads the secrets key serve uses, and the previous one during a
// rotation.
func loadBox(getenv func(string) string) (*secrets.Box, error) {
	box, err := secrets.Load(orDefault(getenv("CLIPMUX_DATA_DIR"), config.DefaultDataDir), getenv("CLIPMUX_SECRETS_KEY"))
	if err != nil {
		return nil, err
	}
	if prev := getenv("CLIPMUX_SECRETS_KEY_PREVIOUS"); prev != "" {
		if err := box.AddPrevious(prev); err != nil {
			return nil, err
		}
	}
	return box, nil
}

// keysCmd runs the key commands (D071).
func keysCmd(cmd string, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	appName := fs.String("app", "", "app name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	needApp := cmd == "rotate-signing-key" || cmd == "list-keys"
	if (needApp && *appName == "") || (cmd == "revoke-key" && fs.NArg() != 1) || (cmd != "revoke-key" && fs.NArg() > 0) {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	switch cmd {
	case "reseal-secrets":
		err = resealSecrets(ctx, pool, getenv, stdout)
	case "rotate-signing-key":
		err = rotateSigningKey(ctx, pool, *appName, stdout)
	case "list-keys":
		err = listKeys(ctx, pool, *appName, stdout)
	case "revoke-key":
		err = revokeKey(ctx, pool, fs.Arg(0), stdout)
	case "reset-totp":
		var n int64
		if n, err = db.New(pool).DisableOwnerTOTP(ctx, project); err == nil {
			fmt.Fprintf(stdout, "second factor turned off (%d owner); turn it on again from the dashboard\n", n)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin: %v\n", err)
		return 1
	}
	return 0
}

func resealSecrets(ctx context.Context, pool *pgxpool.Pool, getenv func(string) string, out io.Writer) error {
	box, err := loadBox(getenv)
	if err != nil {
		return err
	}
	if err := secrets.CheckVersions(ctx, pool, box); err != nil {
		return err
	}
	counts, err := secrets.Reseal(ctx, pool, box)
	for _, c := range secrets.Columns {
		if n := counts[c.Table+"."+c.Column]; n > 0 {
			fmt.Fprintf(out, "%-45s %d resealed\n", c.Table+"."+c.Column, n)
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "every secret is sealed with key v%d; CLIPMUX_SECRETS_KEY_PREVIOUS can go\n", box.Version())
	return nil
}

func rotateSigningKey(ctx context.Context, pool *pgxpool.Pool, appName string, out io.Writer) error {
	var app, kid, privatePEM string
	var retired int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if app, err = appID(ctx, q, appName); err != nil {
			return err
		}
		if kid, privatePEM, err = signingkeys.CreateForApp(ctx, tx, project, app); err != nil {
			return err
		}
		if retired, err = q.RetireOtherSigningKeys(ctx, db.RetireOtherSigningKeysParams{AppID: pgtype.Text{String: app, Valid: true}, KeepID: kid}); err != nil {
			return err
		}
		return routes.QueueKeys(ctx, q, project) // _keys.json carries not_after
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app: %s (%s)\nkid: %s\n\n%s\nSign new playback tokens with this key. %d older key(s) keep working until the\nlongest token they signed expires, then stop.\n",
		appName, app, kid, privatePEM, retired)
	return nil
}

func listKeys(ctx context.Context, pool *pgxpool.Pool, appName string, out io.Writer) error {
	q := db.New(pool)
	app, err := q.GetAppByName(ctx, db.GetAppByNameParams{ProjectID: project, Name: appName})
	if err != nil {
		return fmt.Errorf("app %q: %w", appName, err)
	}
	when := func(t pgtype.Timestamptz) string {
		if !t.Valid {
			return "-"
		}
		return t.Time.UTC().Format(time.RFC3339)
	}
	keys, err := q.ListAPIKeys(ctx, app.ID)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "API keys")
	for _, k := range keys {
		fmt.Fprintf(out, "  %s  %-16s %-40s used %s  revoked %s\n", k.ID, k.Name, strings.Join(k.Scopes, ","), when(k.LastUsedAt), when(k.RevokedAt))
	}
	sk, err := q.ListSigningKeys(ctx, pgtype.Text{String: app.ID, Valid: true})
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Signing keys")
	for _, k := range sk {
		fmt.Fprintf(out, "  %s  created %s  retires %s  revoked %s\n", k.ID, when(k.CreatedAt), when(k.RetiredAt), when(k.RevokedAt))
	}
	return nil
}

func revokeKey(ctx context.Context, pool *pgxpool.Pool, id string, out io.Writer) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{ProjectID: project, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			if n, err = q.RevokeSigningKey(ctx, db.RevokeSigningKeyParams{ProjectID: project, ID: id}); err != nil {
				return err
			}
			if n > 0 {
				if err := routes.QueueKeys(ctx, q, project); err != nil {
					return err
				}
			}
		}
		if n == 0 {
			return fmt.Errorf("no API key or app signing key %s", id)
		}
		fmt.Fprintf(out, "%s revoked\n", id)
		return nil
	})
}
