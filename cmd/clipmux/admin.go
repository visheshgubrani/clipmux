package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
)

const adminUsage = `Usage:
  clipmux admin create-key --app NAME [--name KEY_NAME] [--scopes uploads:write,assets:read]

Host-side administration. Reads DATABASE_URL. create-key creates the app if
it does not exist and prints the key once.
`

// admin runs host-side administration commands against the database.
func admin(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "create-key" {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "app name (created if missing)")
	name := fs.String("name", "default", "key name")
	scopes := fs.String("scopes", apikeys.UploadsWrite+","+apikeys.AssetsRead, "comma-separated scopes")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *app == "" {
		fmt.Fprint(stderr, "clipmux admin create-key: --app is required\n\n"+adminUsage)
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
	if err := createKey(ctx, pool, *app, *name, strings.Split(*scopes, ","), stdout); err != nil {
		fmt.Fprintf(stderr, "clipmux admin create-key: %v\n", err)
		return 1
	}
	return 0
}

// createKey finds or creates the app and issues a key for it.
func createKey(ctx context.Context, pool *pgxpool.Pool, appName, keyName string, scopes []string, out io.Writer) error {
	const project = "prj_default"
	var secret, appID string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		app, err := q.GetAppByName(ctx, db.GetAppByNameParams{ProjectID: project, Name: appName})
		if errors.Is(err, pgx.ErrNoRows) {
			app, err = q.CreateApp(ctx, db.CreateAppParams{ID: ids.New(ids.App), ProjectID: project, Name: appName})
		}
		if err != nil {
			return err
		}
		appID = app.ID
		secret, _, err = apikeys.Create(ctx, tx, project, app.ID, keyName, scopes)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "app:    %s (%s)\nscopes: %s\nkey:    %s\n\nThe key is shown once. Store it in your app's backend.\n",
		appName, appID, strings.Join(scopes, ","), secret)
	return nil
}
