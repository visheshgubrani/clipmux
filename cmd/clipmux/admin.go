package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

const adminUsage = `Usage:
  clipmux admin create-key --app NAME [--name KEY_NAME] [--scopes uploads:write,assets:read]
  clipmux admin create-signing-key --app NAME

Host-side administration until the dashboard exists. Reads DATABASE_URL.
Both commands create the app if it does not exist and print the secret once:
create-key an API key, create-signing-key the private key your backend signs
playback tokens with (only the public key is stored).
`

const project = "prj_default"

// admin runs host-side administration commands against the database.
func admin(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "create-key" && args[0] != "create-signing-key") {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "app name (created if missing)")
	name := fs.String("name", "default", "key name (create-key)")
	scopes := fs.String("scopes", apikeys.UploadsWrite+","+apikeys.AssetsRead, "comma-separated scopes (create-key)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *app == "" {
		fmt.Fprintf(stderr, "clipmux admin %s: --app is required\n\n%s", cmd, adminUsage)
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
	if cmd == "create-key" {
		err = createKey(ctx, pool, *app, *name, strings.Split(*scopes, ","), stdout)
	} else {
		err = createSigningKey(ctx, pool, *app, stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "clipmux admin %s: %v\n", cmd, err)
		return 1
	}
	return 0
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
	privatePEM, publicPEM, err := tokens.GenerateKey()
	if err != nil {
		return err
	}
	kid := ids.New(ids.SigningKey)
	var app string
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		if app, err = appID(ctx, q, appName); err != nil {
			return err
		}
		_, err = q.CreateSigningKey(ctx, db.CreateSigningKeyParams{
			ID: kid, ProjectID: project, AppID: pgtype.Text{String: app, Valid: true}, PublicKeyPem: publicPEM,
		})
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app: %s (%s)\nkid: %s\n\n%s\nSign playback tokens with this private key (ES256, kid %s).\nIt is shown once and not stored by ClipMux.\n",
		appName, app, kid, privatePEM, kid)
	return nil
}
