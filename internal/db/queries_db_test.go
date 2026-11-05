package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/ids"
)

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func TestAssetsAreScopedToTheirAppAndFoundByPassthrough(t *testing.T) {
	ctx := context.Background()
	q := db.New(dbtest.New(t))

	dashboard, err := q.GetBuiltinApp(ctx, "prj_default")
	if err != nil || dashboard.ID != "app_dashboard" {
		t.Fatalf("builtin app = %+v, %v", dashboard, err)
	}
	storage, err := q.GetDefaultStorage(ctx, "prj_default")
	if err != nil || storage.Kind != "fs" {
		t.Fatalf("default storage = %+v, %v", storage, err)
	}
	lms, err := q.CreateApp(ctx, db.CreateAppParams{ID: ids.New(ids.App), ProjectID: "prj_default", Name: "LMS"})
	if err != nil {
		t.Fatal(err)
	}

	create := func(appID, passthrough string) db.Asset {
		t.Helper()
		a, err := q.CreateAsset(ctx, db.CreateAssetParams{
			ID: ids.New(ids.Asset), ProjectID: "prj_default", AppID: appID, StorageID: storage.ID, Passthrough: text(passthrough),
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := create(lms.ID, "lesson-42")
	second := create(lms.ID, "lesson-42")
	create(dashboard.ID, "lesson-42") // same passthrough, other app

	if first.Status != "waiting_upload" || first.Version != 1 {
		t.Fatalf("new asset = status %q version %d, want waiting_upload 1", first.Status, first.Version)
	}

	got, err := q.ListAssetsByPassthrough(ctx, db.ListAssetsByPassthroughParams{AppID: lms.ID, Passthrough: text("lesson-42"), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("passthrough lookup returned %d assets, want the LMS's two, newest first", len(got))
	}

	// Another app cannot read the LMS's asset.
	if _, err := q.GetAsset(ctx, db.GetAssetParams{ID: first.ID, AppID: dashboard.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-app read: err = %v, want no rows", err)
	}
	if a, err := q.GetAsset(ctx, db.GetAssetParams{ID: first.ID, AppID: lms.ID}); err != nil || a.ID != first.ID {
		t.Fatalf("own read: %v", err)
	}
}
