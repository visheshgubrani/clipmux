package cloudflare

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// D099: disconnecting returns delivery to the built-in handler and the
// media URL to the public URL. The R2 store stays, and the bucket loses
// the Worker's records so the Worker refuses what it no longer hears about.
func TestDisconnectReturnsToBuiltInDelivery(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("connect: %+v, %v", res.Steps, err)
	}
	def, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID)
	bucket, _ := r.stash.Get(ctx, def.ID)
	if err := bucket.Put(ctx, routes.RouteKey("pid_1"), strings.NewReader(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := bucket.Put(ctx, "media/ast_1/out_1/master.m3u8", strings.NewReader("#EXTM3U")); err != nil {
		t.Fatal(err)
	}

	res, err := r.conn.Disconnect(ctx)
	if err != nil || !res.Disconnected {
		t.Fatalf("disconnect: %+v, %v", res.Steps, err)
	}
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "media_url,delivery,routes,forgotten" {
		t.Fatalf("steps = %s", got)
	}
	if r.urls.MediaURL() != "https://video.example.org" {
		t.Fatalf("media URL = %s, want the public URL", r.urls.MediaURL())
	}
	if p, _ := r.pool.GetProjectDelivery(ctx, db.DefaultProjectID); p.Delivery != "builtin" || p.DeliveryStorageID.Valid {
		t.Fatalf("delivery = %+v, want builtin", p)
	}
	for _, key := range []string{routes.RouteKey("pid_1"), routes.KeysKey} {
		if _, err := bucket.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("%s after disconnect: %v, want it gone", key, err)
		}
	}
	if _, err := bucket.Head(ctx, "media/ast_1/out_1/master.m3u8"); err != nil {
		t.Fatalf("an asset's files must stay: %v", err)
	}
	if after, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID); after.ID != def.ID {
		t.Fatalf("default storage = %s, want the R2 store %s kept", after.ID, def.ID)
	}
	if st, err := r.conn.Status(ctx); err != nil || st.Connected {
		t.Fatalf("status = %+v, %v; want not connected", st, err)
	}
	if _, err := r.conn.Disconnect(ctx); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a second disconnect = %v, want ErrNotConnected", err)
	}
	// Connecting again finds the same R2 config.
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("reconnect: %+v, %v", res.Steps, err)
	}
	if configs, _ := r.pool.ListStorageConfigs(ctx, db.DefaultProjectID); len(configs) != 2 {
		t.Fatalf("storage configs = %d, want fs and the one r2", len(configs))
	}
}

func TestDisconnectWaitsForAPinnedMediaURL(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("connect: %+v, %v", res.Steps, err)
	}
	pinned, err := settings.Load(ctx, r.raw, db.DefaultProjectID, config.Config{PublicURL: "https://video.example.org", MediaURL: "https://media.example.com", URLsPinned: true})
	if err != nil {
		t.Fatal(err)
	}
	r.conn.URLs = pinned
	if _, err := r.conn.Disconnect(ctx); !errors.Is(err, ErrMediaURLPinned) {
		t.Fatalf("disconnect with CLIPMUX_MEDIA_URL on the Worker = %v, want ErrMediaURLPinned", err)
	}
	if p, _ := r.pool.GetProjectDelivery(ctx, db.DefaultProjectID); p.Delivery != "worker" {
		t.Fatalf("delivery = %s; a refused disconnect changes nothing", p.Delivery)
	}
}
