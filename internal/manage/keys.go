package manage

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/cloudflare"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// ContentKeys lets the owner rotate the content key secret; serve sets it.
type ContentKeys struct {
	// Ring is the ring every holder shares; a rotation replaces it.
	Ring contentkey.Ring
	// DataDir is where an older install kept content.key, for Load.
	DataDir string
}

// rotateContentKey adds a content key secret version and makes new outputs
// use it (D071). With Worker delivery the Worker must hold a version before
// any output uses it, so this needs a Cloudflare token: the new version is
// added pending, the Worker is redeployed with the whole ring through
// Connect Cloudflare, and only then is the version made usable. Existing
// outputs keep their own versions, so nothing stops playing; re-encode an
// output to move it onto the new secret.
func (m *API) rotateContentKey(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Keys == nil {
		return httpjson.Fail(http.StatusNotFound, "not_found", "not found")
	}
	var body struct {
		CloudflareToken string `json:"cloudflare_token"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	ctx := r.Context()
	delivery, err := db.New(m.pool).GetProjectDelivery(ctx, o.ProjectID)
	if err != nil {
		return err
	}
	worker := delivery.Delivery == "worker"
	if worker && (body.CloudflareToken == "" || m.Cloudflare == nil) {
		return httpjson.Fail(http.StatusConflict, "cloudflare_token_required",
			"the Cloudflare Worker must hold a new secret before outputs use it: pass cloudflare_token, as for Connect Cloudflare")
	}
	var version int32
	if err := pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		version, err = contentkey.AddVersion(ctx, tx, m.box, o.ProjectID)
		return err
	}); err != nil {
		return err
	}
	if err := m.reloadKeys(ctx, o.ProjectID); err != nil {
		return err
	}
	var steps []cloudflare.Step
	if worker {
		st, err := m.Cloudflare.Status(ctx)
		if err != nil {
			return err
		}
		res, err := m.Cloudflare.Connect(ctx, cloudflare.Request{Token: body.CloudflareToken, Hostname: st.Hostname, Bucket: st.Bucket, AccountID: st.AccountID})
		if err != nil {
			return err
		}
		if !res.Connected {
			// The version stays pending, unused; rotating again retries.
			httpjson.Write(w, http.StatusBadGateway, map[string]any{"version": version, "usable": false, "steps": res.Steps})
			return nil
		}
		steps = res.Steps
	}
	if err := contentkey.MakeUsable(ctx, m.pool, o.ProjectID); err != nil {
		return err
	}
	if err := m.reloadKeys(ctx, o.ProjectID); err != nil {
		return err
	}
	m.Logger.InfoContext(ctx, "content key secret rotated", "version", version, "worker_redeployed", worker)
	httpjson.Write(w, http.StatusOK, map[string]any{"version": version, "usable": true, "current": m.Keys.Ring.Current().Version, "steps": steps})
	return nil
}

func (m *API) reloadKeys(ctx context.Context, projectID string) error {
	ring, err := contentkey.Load(ctx, m.pool, m.box, projectID, m.Keys.DataDir)
	if err != nil {
		return fmt.Errorf("reload content key secrets: %w", err)
	}
	m.Keys.Ring.Replace(ring)
	return nil
}
