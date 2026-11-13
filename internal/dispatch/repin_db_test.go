package dispatch_test

import (
	"context"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
)

// An upgraded server moves a provider that runs its default image to the
// new build's digest, keeping it verified; an image the owner chose stays.
func TestRepinMovesOnlyTheDefaultImage(t *testing.T) {
	r := newRig(t)
	reg := registrytest.NewPublic(t)
	res := &registry.Resolver{HTTP: reg.Client}
	ctx := context.Background()
	old := reg.Host + "/" + registrytest.Repo + "@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"
	row := func() db.ExecutorConfig {
		t.Helper()
		row, err := db.New(r.pool).GetExecutorConfigByKind(ctx, db.GetExecutorConfigByKindParams{ProjectID: "prj_default", Kind: "runpod"})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	r.exec(`UPDATE executor_configs SET config = jsonb_build_object('image', $1::text, 'image_chosen', true)`, old)
	if pinned, err := dispatch.Repin(ctx, r.pool, res, row(), reg.Image(), "", ""); err != nil || pinned != "" {
		t.Fatalf("an owner's image: Repin = %q, %v; want it left alone", pinned, err)
	}

	r.exec(`UPDATE executor_configs SET config = jsonb_build_object('image', $1::text, 'gpu', 'L4')`, old)
	before := row()
	pinned, err := dispatch.Repin(ctx, r.pool, res, before, reg.Image(), "", "")
	if err != nil || pinned != reg.Pinned() {
		t.Fatalf("Repin = %q, %v; want %s", pinned, err, reg.Pinned())
	}
	after := row()
	if string(after.Config) != `{"gpu": "L4", "image": "`+reg.Pinned()+`"}` || !after.VerifiedAt.Valid || !after.UpdatedAt.Time.After(before.UpdatedAt.Time) {
		t.Fatalf("after: config %s, verified %v, updated %v (was %v)", after.Config, after.VerifiedAt.Valid, after.UpdatedAt.Time, before.UpdatedAt.Time)
	}
	// Pinned already: nothing moves.
	if pinned, err := dispatch.Repin(ctx, r.pool, res, after, reg.Image(), "", ""); err != nil || pinned != "" {
		t.Fatalf("second Repin = %q, %v; want nothing", pinned, err)
	}
	// Another repository (an owner's fork, connected before image_chosen
	// existed) isn't this build's image.
	if dispatch.SameRepository(reg.Pinned(), "ghcr.io/someone/clipmux:sha-abc") {
		t.Fatal("different repositories compared equal")
	}
}
