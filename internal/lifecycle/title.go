package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/outbox"
)

// SetTitle names one of the key's app's assets, or clears its name with an
// empty title (D099). The version moves and video.asset.updated says so,
// like any other change an app may have missed.
func (s *Service) SetTitle(ctx context.Context, key apikeys.Key, assetID, title string) (assets.View, error) {
	t, err := assets.Title(title)
	if err != nil {
		return assets.View{}, err
	}
	var out assets.View
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		asset, err := s.lockOwnAsset(ctx, q, key, assetID)
		if err != nil {
			return err
		}
		if asset.Status == "deleted" {
			return errAssetDeleted()
		}
		if asset.Title == t {
			out, err = assets.Load(ctx, q, asset)
			return err
		}
		if asset, err = q.SetAssetTitle(ctx, db.SetAssetTitleParams{ID: assetID, Title: t}); err != nil {
			return err
		}
		if err := outbox.AssetEvent(ctx, q, assetID, outbox.AssetUpdated, nil); err != nil {
			return err
		}
		out, err = assets.Load(ctx, q, asset)
		return err
	})
	return out, err
}

// handlePatch is PATCH /v1/assets/{id}: {title}, where "" or null clears it.
func (s *Service) handlePatch(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	var body struct {
		Title json.RawMessage `json:"title"`
	}
	if !httpjson.Decode(w, r, &body) {
		return
	}
	var title *string
	if len(body.Title) == 0 || json.Unmarshal(body.Title, &title) != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "give title: a string, or null to clear it")
		return
	}
	if title == nil {
		title = new(string)
	}
	v, err := s.SetTitle(r.Context(), key, r.PathValue("id"), *title)
	s.respond(w, "set_title", http.StatusOK, v, err)
}
