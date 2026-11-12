package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// TestP7LiveEdge is P7-T10 on the retained staging zone (D090): playback
// through the real Worker reaches /v1/analytics after a real edge pull with
// a read-only token, and a revoked share link stops at the edge within 65
// seconds. Opt-in, like TestCloudflareLiveAcceptance; it keeps the install.
//
// It needs, beside live.json and owner.json, a second Cloudflare token in
// the private state directory, analytics-token (0600), with only Account
// Analytics: Read. Nothing it prints holds a token, secret or signed URL.
func TestP7LiveEdge(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_LIVE_P7") != "1" {
		t.Skip("the P7 live run is opt-in")
	}
	path := os.Getenv("CLIPMUX_E2E_LIVE_CONFIG")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private live config unavailable")
	}
	var cfg liveFixture
	if json.Unmarshal(raw, &cfg) != nil || cfg.API != "https://app-test.clipmux.com" || cfg.BaseURL != "https://media-test.clipmux.com" {
		t.Fatal("unexpected live staging config")
	}
	state := filepath.Dir(path)
	database := os.Getenv("CLIPMUX_E2E_LIVE_DATABASE_URL")
	if database != "postgres://clipmux:clipmux@127.0.0.1:5445/clipmux?sslmode=disable" {
		t.Fatal("dedicated staging database required")
	}
	pool, err := pgxpool.New(t.Context(), database)
	if err != nil {
		t.Fatal("staging database unavailable")
	}
	defer pool.Close()
	in := &instance{t: t, url: cfg.API, pool: pool}
	var ownerBody map[string]string
	if raw, err = os.ReadFile(filepath.Join(state, "owner.json")); err != nil || json.Unmarshal(raw, &ownerBody) != nil {
		t.Fatal("owner unavailable")
	}
	jar, _ := cookiejar.New(nil)
	owner := &http.Client{Jar: jar, Timeout: 3 * time.Minute}
	if s := ownerCall(t, owner, "POST", cfg.API+"/v1/session", ownerBody, nil); s != 200 {
		t.Fatalf("owner login HTTP %d", s)
	}

	// 1. The Worker this build ships, with the analytics bindings.
	var cf struct {
		Connected     bool `json:"connected"`
		NeedsRedeploy bool `json:"needs_redeploy"`
	}
	if s := ownerCall(t, owner, "GET", cfg.API+"/v1/cloudflare", nil, &cf); s != 200 || !cf.Connected {
		t.Fatalf("Cloudflare status HTTP %d, connected %v", s, cf.Connected)
	}
	if cf.NeedsRedeploy {
		token, err := os.ReadFile(filepath.Join(state, "cloudflare-token"))
		if err != nil {
			t.Fatal("Cloudflare token unavailable")
		}
		var res struct {
			Connected bool `json:"connected"`
			Steps     []struct {
				Name string `json:"name"`
				OK   bool   `json:"ok"`
			} `json:"steps"`
		}
		s := ownerCall(t, owner, "POST", cfg.API+"/v1/cloudflare/connect", map[string]string{
			"token": strings.TrimSpace(string(token)), "hostname": "media-test.clipmux.com", "bucket": "clipmux-staging",
		}, &res)
		if s != 200 || !res.Connected {
			failed := []string{}
			for _, st := range res.Steps {
				if !st.OK {
					failed = append(failed, st.Name)
				}
			}
			t.Fatalf("redeploy HTTP %d, failed steps %v", s, failed)
		}
		t.Log("Worker redeployed with the analytics bindings")
	}

	// 2. Edge analytics with the read-only token. The first connect starts 13
	// days back and catches up a day per 5-minute pass; there is no edge
	// history on staging before this build, so the watermark is moved to the
	// last hour to skip an hour of empty passes (the catch-up is covered by
	// internal/analytics against a fake SQL API).
	readToken, err := os.ReadFile(filepath.Join(state, "analytics-token"))
	if err != nil {
		t.Fatal("read-only analytics token unavailable: save it as analytics-token in the private state directory")
	}
	if s := ownerCall(t, owner, "POST", cfg.API+"/v1/cloudflare/analytics", map[string]string{"token": strings.TrimSpace(string(readToken))}, nil); s != 200 {
		t.Fatalf("connect edge analytics HTTP %d", s)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE edge_analytics SET pulled_through = date_trunc('hour', now()) - interval '1 hour'`); err != nil {
		t.Fatal("move the edge watermark")
	}
	// A pull after this point has taken in every earlier edge play (an
	// earlier run's included), so the baseline below holds still.
	settled := time.Now()
	for {
		var st struct {
			LastPullAt *time.Time `json:"last_pull_at"`
			LastError  string     `json:"last_error"`
		}
		if s := ownerCall(t, owner, "GET", cfg.API+"/v1/cloudflare/analytics", nil, &st); s == 200 && st.LastPullAt != nil && st.LastPullAt.After(settled.Add(time.Minute)) {
			if st.LastError != "" {
				t.Fatal("the edge pull failed; its error is in the staging dashboard")
			}
			break
		}
		if time.Since(settled) > 8*time.Minute {
			t.Fatal("no edge pull completed within 8 minutes")
		}
		time.Sleep(15 * time.Second)
	}
	before := assetTotals(t, owner, cfg)
	if before.Collecting.Edge != "connected" {
		t.Fatalf("collecting.edge = %q, want connected", before.Collecting.Edge)
	}

	// 3. A scripted viewer through the Worker, with a vid.
	scriptStart := time.Now().Add(-2 * time.Second)
	vid := "p7live-" + randHex(8)
	tok := signVid(t, cfg, cfg.PlaybackID, vid)
	base := cfg.BaseURL + "/v/" + tok + "/" + cfg.PlaybackID + "/"
	var want counts
	fetch := func(u string) []byte {
		status, body := liveGet(t, u)
		if status != 200 {
			t.Fatalf("delivery HTTP %d for a %s", status, kindOfURL(u))
		}
		want.bytes += int64(len(body))
		return body
	}
	master := string(fetch(base + "master.m3u8"))
	want.views++
	audio := regexp.MustCompile(`TYPE=AUDIO[^\n]*URI="([^"]+)"`).FindStringSubmatch(master)
	video := regexp.MustCompile(`(?m)^([^#\s][^\s]*index\.m3u8)$`).FindStringSubmatch(master)
	if audio == nil || video == nil {
		t.Fatal("master lists no audio or video playlist")
	}
	for i, playlist := range []string{audio[1], video[1]} {
		dir := playlist[:strings.LastIndex(playlist, "/")+1]
		body := string(fetch(base + playlist))
		if m := regexp.MustCompile(`#EXT-X-MAP:URI="([^"]+)"`).FindStringSubmatch(body); m != nil {
			fetch(base + dir + m[1])
		}
		if m := regexp.MustCompile(`#EXT-X-KEY:[^\n]*URI="([^"]+)"`).FindStringSubmatch(body); m != nil && i == 0 {
			ref, _ := url.Parse(base + dir)
			key, _ := ref.Parse(m[1])
			fetch(key.String())
		}
		segs := regexp.MustCompile(`(?m)^(seg_\d+\.m4s)$`).FindAllStringSubmatch(body, -1)
		n := 1
		if i == 0 {
			n = 2 // audio carries the watch time
			want.watch += 12
		}
		if len(segs) < n {
			t.Fatal("playlist lists too few segments")
		}
		for _, s := range segs[:n] {
			fetch(base + dir + s[1])
		}
	}
	want.viewers++

	// 4. A share link plays through the Worker, then is revoked there.
	var link struct {
		ID         string `json:"id"`
		URL        string `json:"url"`
		PlaybackID string `json:"playback_id"`
	}
	if s := in.call("POST", "/v1/assets/"+cfg.AssetID+"/shares", cfg.APIKey, map[string]string{"name": "P7 live"}, &link); s != 201 {
		t.Fatalf("create share HTTP %d", s)
	}
	secret := link.URL[strings.LastIndex(link.URL, "/s/")+3:]
	var play struct {
		Token      string `json:"token"`
		BaseURL    string `json:"base_url"`
		PlaybackID string `json:"playback_id"`
	}
	if s := livePlayback(t, cfg.API+"/s/"+secret+"/playback", randHex(16), &play); s != 200 || play.BaseURL != cfg.BaseURL {
		t.Fatalf("share playback HTTP %d", s)
	}
	want.viewers++
	shareMaster := play.BaseURL + "/v/" + play.Token + "/" + play.PlaybackID + "/master.m3u8"
	shareViews := int64(0)
	// The ID's route reaches the Worker's bucket on publish; wait for it.
	begin := time.Now()
	for {
		status, body := liveGet(t, shareMaster)
		if status == 200 {
			shareViews++
			want.bytes += int64(len(body))
			break
		}
		if time.Since(begin) > 65*time.Second {
			t.Fatalf("a new share link answers HTTP %d at the edge after 65 s", status)
		}
		time.Sleep(2 * time.Second)
	}
	if s := in.call("DELETE", "/v1/shares/"+link.ID, cfg.APIKey, nil, nil); s != 200 {
		t.Fatalf("revoke share HTTP %d", s)
	}
	revoked := time.Now()
	for {
		status, body := liveGet(t, shareMaster)
		if status == 404 {
			break
		}
		if status != 200 {
			t.Fatalf("a revoking share link answers HTTP %d at the edge", status)
		}
		shareViews++
		want.bytes += int64(len(body))
		if time.Since(revoked) > 65*time.Second {
			t.Fatal("a revoked share link still plays at the edge after 65 s")
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("revoked share link stopped at the edge after %s", time.Since(revoked).Round(time.Second))
	want.views += shareViews

	// 5. The next pulls bring those counts in. Analytics Engine samples
	// even one viewer's burst (D106), so the pull must equal its weighted
	// sums exactly, and the script within 1% (at least one whole unit).
	deadline := time.Now().Add(20 * time.Minute)
	for {
		now := assetTotals(t, owner, cfg)
		got := counts{
			views: now.Totals.Views - before.Totals.Views, watch: now.Totals.WatchSeconds - before.Totals.WatchSeconds,
			bytes: now.Totals.Bytes - before.Totals.Bytes, viewers: deref(now.Totals.UniqueViewers) - deref(before.Totals.UniqueViewers),
		}
		edge := edgeSums(t, strings.TrimSpace(string(readToken)), cfg.AssetID, scriptStart)
		if got == edge && within(got, want) {
			t.Logf("pulled counts equal Analytics Engine's weighted sums %+v; the script made %+v", edge, want)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 20 minutes: pulled %+v, Analytics Engine %+v, script %+v", got, edge, want)
		}
		time.Sleep(30 * time.Second)
	}
	var shares struct {
		Data []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Views30d int64  `json:"views_30d"`
		} `json:"data"`
	}
	if s := in.call("GET", "/v1/assets/"+cfg.AssetID+"/shares", cfg.APIKey, nil, &shares); s != 200 {
		t.Fatalf("list shares HTTP %d", s)
	}
	for _, l := range shares.Data {
		if l.ID == link.ID && (l.Status != "revoked" || l.Views30d != shareViews) {
			t.Fatalf("share link reads %s with %d views, want revoked with %d", l.Status, l.Views30d, shareViews)
		}
	}
	t.Logf("the share link's %d views matched", shareViews)
}

type counts struct{ views, watch, bytes, viewers int64 }

// within reports whether got is within 1% of want in every count, rounded
// up to at least one whole unit.
func within(got, want counts) bool {
	near := func(g, w int64) bool {
		tol := max((w+99)/100, 1)
		return g >= w-tol && g <= w+tol
	}
	return near(got.views, want.views) && near(got.watch, want.watch) && near(got.bytes, want.bytes) && near(got.viewers, want.viewers)
}

// edgeSums asks Analytics Engine itself, with the read-only token, for the
// asset's weighted sums since a time: what the pull must have taken in.
func edgeSums(t *testing.T, token, assetID string, since time.Time) counts {
	t.Helper()
	q := fmt.Sprintf(`SELECT SUM(_sample_interval * double1) AS v, SUM(_sample_interval * double2) AS w,
		SUM(_sample_interval * double3) AS b, COUNT(DISTINCT blob6) AS n, countIf(blob6 = '') AS anon
		FROM clipmux_delivery WHERE blob1 = 'prj_default' AND blob3 = '%s' AND timestamp >= toDateTime(%d) FORMAT JSON`,
		assetID, since.Unix())
	req, err := http.NewRequestWithContext(t.Context(), "POST",
		"https://api.cloudflare.com/client/v4/accounts/49b962322ece3363f6495da349dd9d9e/analytics_engine/sql", strings.NewReader(q))
	if err != nil {
		t.Fatal("invalid SQL API request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("SQL API transport failed")
	}
	defer res.Body.Close()
	var out struct {
		Data []map[string]json.Number `json:"data"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&out) != nil || len(out.Data) != 1 {
		t.Fatalf("SQL API HTTP %d", res.StatusCode)
	}
	n := func(k string) int64 {
		f, _ := out.Data[0][k].Float64()
		return int64(f + 0.5)
	}
	viewers := n("n")
	if n("anon") > 0 {
		viewers-- // the empty key of points without a vid
	}
	return counts{views: n("v"), watch: n("w"), bytes: n("b"), viewers: viewers}
}

type liveTotals struct {
	Totals struct {
		Views         int64  `json:"views"`
		WatchSeconds  int64  `json:"watch_seconds"`
		Bytes         int64  `json:"bytes_delivered"`
		UniqueViewers *int64 `json:"unique_viewers"`
	} `json:"totals"`
	Collecting struct {
		Edge string `json:"edge"`
	} `json:"collecting"`
}

func assetTotals(t *testing.T, owner *http.Client, cfg liveFixture) liveTotals {
	t.Helper()
	// Through the Tunnel, so a transient 5xx from the edge is retried.
	for try := 0; ; try++ {
		var out liveTotals
		s := ownerCall(t, owner, "GET", cfg.API+"/v1/analytics/assets/"+cfg.AssetID+"?range=24h", nil, &out)
		if s == 200 {
			return out
		}
		if s < 500 || try == 4 {
			t.Fatalf("asset analytics HTTP %d", s)
		}
		time.Sleep(5 * time.Second)
	}
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error.
	return hex.EncodeToString(b)
}

func signVid(t *testing.T, cfg liveFixture, pid, vid string) string {
	t.Helper()
	key, err := tokens.ParsePrivateKey(cfg.PrivateKey)
	if err != nil {
		t.Fatal("invalid private signing key")
	}
	now := time.Now().Unix()
	tok, err := tokens.Sign(key, cfg.KID, tokens.Claims{Sub: pid, Aud: tokens.AudiencePlayback, Iat: now, Exp: now + 3600, Vid: vid})
	if err != nil {
		t.Fatal("sign")
	}
	return tok
}

// liveGet fetches through the real edge; errors never carry the URL.
func liveGet(t *testing.T, u string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET", u, nil)
	if err != nil {
		t.Fatal("invalid delivery request")
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("delivery transport failed; URL withheld")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		t.Fatal("delivery body failed")
	}
	return res.StatusCode, body
}

// livePlayback asks a share link's public endpoint for a token; errors
// never carry the URL, which holds the link's secret.
func livePlayback(t *testing.T, u, viewer string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", u, strings.NewReader(`{"viewer_id":"`+viewer+`"}`))
	if err != nil {
		t.Fatal("invalid share request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("share transport failed; URL withheld")
	}
	defer res.Body.Close()
	_ = json.NewDecoder(res.Body).Decode(out)
	return res.StatusCode
}

// kindOfURL names what a URL fetched, without its token.
func kindOfURL(u string) string {
	return fmt.Sprintf("%q", u[strings.LastIndex(u, "/")+1:])
}
