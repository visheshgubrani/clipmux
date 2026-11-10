package lifecycle_test

import (
	"net/http"
	"strings"
	"testing"
)

// D099: an asset's title is given at import or upload and changed with
// PATCH; each change is a new version with video.asset.updated.
func TestATitleIsGivenAtImportAndRenamedWithPatch(t *testing.T) {
	r := newRig(t)
	r.reachLocal()
	src, _ := origin(t, http.StatusOK)
	status, view := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/video.mp4"), "title": "  Lesson 9: Fractions  "}, "")
	if status != http.StatusCreated || view["title"] != "Lesson 9: Fractions" {
		t.Fatalf("import with a title = %d %v; want it trimmed", status, view)
	}
	id := view["id"].(string)
	if status, got := r.send(http.MethodPost, "/v1/assets", r.key, map[string]any{"url": localURL(src, "/video.mp4"), "title": strings.Repeat("é", 201)}, ""); status != http.StatusBadRequest {
		t.Fatalf("a 201-character title = %d %v, want 400", status, got)
	}

	status, view = r.send(http.MethodPatch, "/v1/assets/"+id, r.key, map[string]any{"title": "Fractions, part 1"}, "")
	if status != http.StatusOK || view["title"] != "Fractions, part 1" || view["version"] != float64(2) {
		t.Fatalf("rename = %d %v; want the title at version 2", status, view)
	}
	if got := r.events(); len(got) != 1 || got[0] != "video.asset.updated" {
		t.Fatalf("events = %v, want one video.asset.updated", got)
	}
	// The same title again changes nothing.
	if _, view = r.send(http.MethodPatch, "/v1/assets/"+id, r.key, map[string]any{"title": "Fractions, part 1"}, ""); view["version"] != float64(2) || len(r.events()) != 1 {
		t.Fatalf("an unchanged title = %v, events %v; want version 2 and no event", view, r.events())
	}
	status, view = r.send(http.MethodPatch, "/v1/assets/"+id, r.key, map[string]any{"title": nil}, "")
	if _, has := view["title"]; status != http.StatusOK || has || view["version"] != float64(3) {
		t.Fatalf("clear = %d %v; want no title at version 3", status, view)
	}
	for _, bad := range []map[string]any{{}, {"title": 7}, {"title": "two\nlines"}, {"title": strings.Repeat("a", 201)}} {
		if status, got := r.send(http.MethodPatch, "/v1/assets/"+id, r.key, bad, ""); status != http.StatusBadRequest {
			t.Fatalf("PATCH %v = %d %v, want 400", bad, status, got)
		}
	}
	if status, _ := r.send(http.MethodPatch, "/v1/assets/ast_other", r.key, map[string]any{"title": "x"}, ""); status != http.StatusNotFound {
		t.Fatalf("another asset = %d, want 404", status)
	}
	r.exec(`UPDATE assets SET status = 'deleted' WHERE id = $1`, id)
	if status, got := r.send(http.MethodPatch, "/v1/assets/"+id, r.key, map[string]any{"title": "x"}, ""); status != http.StatusConflict || got["error"].(map[string]any)["code"] != "asset_deleted" {
		t.Fatalf("renaming a deleted asset = %d %v, want 409 asset_deleted", status, got)
	}
}
