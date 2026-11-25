// Package setuptest runs the setup test (docs/architecture.md, "Setup
// test"): after first boot or a change of URLs, a sample clip goes through
// the configured path exactly as an app's would. It is uploaded through
// signed part URLs on the public URL, encoded by a worker, and played
// through the media URL with tokens from the server's internal key. A valid
// v token must play and fetch the key, a t token must get only the poster,
// no token must be refused, and the segment in storage must be ciphertext.
// When something can transcribe, the sample's speech must also come back as
// subtitles that play at the right time, stay out of a t token's reach and
// sit encrypted in storage (P5-T19).
//
// Every request goes over HTTP to the URLs the owner configured, so a wrong
// hostname, proxy or certificate fails here rather than in an app.
// CLIPMUX_SELF_DIAL lets a container reach its own public hostname through
// Caddy while keeping that hostname for TLS and routing.
package setuptest

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/ingest"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

// sample is 11 s of synthetic video with real speech
// (toolchain/make_sample.sh).
//
//go:embed sample.mp4
var sample []byte

// Passthrough marks the sample's assets in the library.
const Passthrough = "clipmux-setup-test"

// The sample's speech: "Americans" is heard from 1.22 s to 1.98 s
// (whisper.cpp large-v3-turbo). A cue holding the word must cover
// expectedAtMS within cueSlackMS, whoever transcribed it.
const (
	expectedWord = "americans"
	expectedAtMS = 1600
	cueSlackMS   = 500
)

// Check is one result. Details never hold a URL or a token.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Test is one run.
type Test struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Trigger string `json:"trigger"`
	// Executor is the provider a connect test runs on (D054), if any.
	Executor   string     `json:"executor,omitempty"`
	AssetID    string     `json:"asset_id,omitempty"`
	Checks     []Check    `json:"checks"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func view(t db.SetupTest) Test {
	out := Test{ID: t.ID, Status: t.Status, Trigger: t.Trigger, Executor: t.Executor.String, AssetID: t.AssetID.String, CreatedAt: t.CreatedAt.Time.UTC(), Checks: []Check{}}
	_ = json.Unmarshal(t.Checks, &out.Checks)
	if t.FinishedAt.Valid {
		f := t.FinishedAt.Time.UTC()
		out.FinishedAt = &f
	}
	return out
}

// Runner queues and runs setup tests, one at a time.
type Runner struct {
	pool      *pgxpool.Pool
	stores    *stores.Set
	box       *secrets.Box
	urls      *settings.Store
	uploads   *uploads.Service
	life      *lifecycle.Service
	projectID string
	client    *http.Client

	// ProviderFallback is the fallback a cloud GPU gets when its connect
	// test passes and it becomes the primary (D116): the built-in worker,
	// or "" when serve runs without one.
	ProviderFallback string
	// EncodeTimeout bounds the wait for the sample to become ready.
	EncodeTimeout time.Duration
	Poll          time.Duration
	Logger        *slog.Logger
}

// New returns a runner. selfDial, when set (a host, or host:port), is where
// every request connects, whatever host its URL names; a bare host keeps
// the URL's port.
func New(pool *pgxpool.Pool, set *stores.Set, box *secrets.Box, urls *settings.Store, upl *uploads.Service, life *lifecycle.Service, selfDial string) *Runner {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if selfDial != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !shouldSelfDial(ctx, pool, urls, addr) {
				return dialer.DialContext(ctx, network, addr)
			}
			target := selfDial
			if _, _, err := net.SplitHostPort(selfDial); err != nil {
				_, port, _ := net.SplitHostPort(addr)
				target = net.JoinHostPort(selfDial, port)
			}
			return dialer.DialContext(ctx, network, target)
		}
	}
	return &Runner{
		pool: pool, stores: set, box: box, urls: urls, uploads: upl, life: life, projectID: db.DefaultProjectID,
		client: &http.Client{
			Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		EncodeTimeout: 15 * time.Minute, Poll: time.Second, Logger: slog.New(slog.DiscardHandler),
	}
}

// Queue asks for a test. While one is queued or running, that one is
// returned instead of a second.
func (r *Runner) Queue(ctx context.Context, trigger string) (Test, error) {
	return r.QueueThrough(ctx, trigger, "")
}

// ErrBusy means another setup test is queued or running, through a
// different executor than the one asked for.
var ErrBusy = errors.New("setuptest: another setup test is running")

// QueueThrough asks for a test whose sample runs only on executor (D054):
// after a provider is connected, its own machines must encode the sample,
// and a pass marks the provider verified. An empty executor follows the
// project's routing. While a test is queued or running, that one is
// returned if it goes the same way, and ErrBusy otherwise.
func (r *Runner) QueueThrough(ctx context.Context, trigger, executor string) (Test, error) {
	q := db.New(r.pool)
	t, err := q.QueueSetupTest(ctx, db.QueueSetupTestParams{
		ID: ids.New(ids.SetupTest), ProjectID: r.projectID, Trigger: trigger,
		Executor: pgtype.Text{String: executor, Valid: executor != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		t, err = q.GetActiveSetupTest(ctx, r.projectID)
		if err == nil && t.Executor.String != executor {
			return view(t), ErrBusy
		}
	}
	if err != nil {
		return Test{}, fmt.Errorf("queue setup test: %w", err)
	}
	return view(t), nil
}

// providerPassed records that a provider's own machines encoded the
// sample: it may take work (D056), and it takes it now, as the primary with
// ProviderFallback behind it, since the owner connected it to use it (D116).
func (r *Runner) providerPassed(ctx context.Context, kind string) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.VerifyExecutor(ctx, db.VerifyExecutorParams{ProjectID: r.projectID, Kind: kind}); err != nil {
			return fmt.Errorf("verify executor: %w", err)
		}
		fallback := pgtype.Text{String: r.ProviderFallback, Valid: r.ProviderFallback != ""}
		if err := q.SetRouting(ctx, db.SetRoutingParams{ID: r.projectID, PrimaryExecutor: kind, FallbackExecutor: fallback}); err != nil {
			return fmt.Errorf("route to %s: %w", kind, err)
		}
		r.Logger.Info("cloud GPU in use", "executor", kind, "fallback", r.ProviderFallback)
		return nil
	})
}

// Latest returns the newest test, if any.
func (r *Runner) Latest(ctx context.Context) (Test, bool, error) {
	t, err := db.New(r.pool).LatestSetupTest(ctx, r.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Test{}, false, nil
	}
	if err != nil {
		return Test{}, false, err
	}
	return view(t), true, nil
}

// Get returns one test.
func (r *Runner) Get(ctx context.Context, id string) (Test, error) {
	t, err := db.New(r.pool).GetSetupTest(ctx, id)
	if err != nil {
		return Test{}, err
	}
	return view(t), nil
}

// Run runs the queued test, if there is one, and returns how many ran.
func (r *Runner) Run(ctx context.Context) (int, error) {
	q := db.New(r.pool)
	t, err := q.ClaimSetupTest(ctx, r.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("claim setup test: %w", err)
	}
	r.Logger.Info("setup test started", "test", t.ID, "trigger", t.Trigger)
	checks := r.execute(ctx, t.ID)
	status := "passed"
	for _, c := range checks {
		if !c.OK {
			status = "failed"
		}
	}
	raw, _ := json.Marshal(checks)
	// Record the result even if serve is stopping.
	if err := q.FinishSetupTest(context.WithoutCancel(ctx), db.FinishSetupTestParams{ID: t.ID, Status: status, Checks: raw}); err != nil {
		return 1, fmt.Errorf("record setup test: %w", err)
	}
	if status == "passed" && (t.Executor.String == "modal" || t.Executor.String == "runpod") {
		if err := r.providerPassed(context.WithoutCancel(ctx), t.Executor.String); err != nil {
			return 1, err
		}
	}
	if status == "passed" {
		r.Logger.Info("setup test passed", "test", t.ID)
	} else {
		r.Logger.Error("setup test failed", "test", t.ID, "checks", string(raw))
	}
	return 1, nil
}

// results collects checks; after a failure the rest are skipped.
type results struct {
	checks []Check
	failed string
}

func (rs *results) add(name string, err error) bool {
	switch {
	case rs.failed != "":
		rs.checks = append(rs.checks, Check{Name: name, Detail: "skipped: " + rs.failed + " failed"})
		return false
	case err != nil:
		rs.checks = append(rs.checks, Check{Name: name, Detail: err.Error()})
		rs.failed = name
		return false
	}
	rs.checks = append(rs.checks, Check{Name: name, OK: true})
	return true
}

// skip records checks that can't run because an earlier one failed.
func (rs *results) skip(names ...string) {
	for _, n := range names {
		rs.checks = append(rs.checks, Check{Name: n, Detail: "skipped: " + rs.failed + " failed"})
	}
}

// PlaybackChecks are the names of the delivery checks, in order.
var PlaybackChecks = []string{
	"play_master", "play_key", "play_segment_decrypts",
	"thumbnail_token_refused_segment", "thumbnail_token_refused_key", "thumbnail_token_poster",
	"no_token_refused", "stored_segment_encrypted",
}

// TrackChecks are the subtitles checks, in order. They run when something
// can transcribe as the test starts (a worker or PC with speech, or a speech
// API in use) and pass as skipped otherwise.
var TrackChecks = []string{"subtitles_ready", "play_subtitles", "thumbnail_token_refused_track", "stored_track_encrypted"}

// skippedOK records checks that don't apply here; they don't fail the test.
func (rs *results) skippedOK(why string, names ...string) {
	for _, n := range names {
		rs.checks = append(rs.checks, Check{Name: n, OK: true, Detail: "skipped: " + why})
	}
}

func (r *Runner) execute(ctx context.Context, testID string) []Check {
	rs := &results{}
	r.deleteEarlierSamples(ctx, testID)
	speech, err := db.New(r.pool).ProjectCanTranscribe(ctx, db.ProjectCanTranscribeParams{OnlineSeconds: 60, ProjectID: r.projectID})
	if err != nil {
		r.Logger.Warn("can the project transcribe", "err", err)
	}
	assetID, err := r.upload(ctx, testID, speech)
	if !rs.add("upload", err) {
		rs.skip("encode")
		rs.skip(PlaybackChecks...)
		rs.skip(TrackChecks...)
		return rs.checks
	}
	if !rs.add("encode", r.waitReady(ctx, assetID)) {
		rs.skip(PlaybackChecks...)
		rs.skip(TrackChecks...)
		return rs.checks
	}
	rs.checks = append(rs.checks, r.playbackChecks(ctx, assetID)...)
	if !speech {
		rs.skippedOK("nothing here can transcribe yet (a worker with speech, or a speech API); a new worker downloads its 0.9 GB speech models first, so run the test again in a few minutes", TrackChecks...)
		return rs.checks
	}
	return append(rs.checks, r.trackChecks(ctx, assetID)...)
}

// deleteEarlierSamples removes the samples of earlier tests, so the latest
// one stays playable in the dashboard and the rest don't pile up.
func (r *Runner) deleteEarlierSamples(ctx context.Context, testID string) {
	if r.life == nil {
		return
	}
	q := db.New(r.pool)
	earlier, err := q.ListEarlierSetupTestAssets(ctx, db.ListEarlierSetupTestAssetsParams{ProjectID: r.projectID, ID: testID})
	if err != nil {
		r.Logger.Warn("list earlier samples", "err", err)
		return
	}
	app, err := q.GetBuiltinApp(ctx, r.projectID)
	if err != nil {
		return
	}
	key := apikeys.Key{ProjectID: r.projectID, AppID: app.ID, Scopes: apikeys.AllScopes(), Owner: true}
	for _, id := range earlier {
		if _, _, err := r.life.Delete(ctx, key, id); err != nil {
			r.Logger.Warn("delete earlier sample", "asset", id, "err", err)
		}
	}
}

// upload creates the sample's upload in-process, as an app's backend would,
// then does what a browser does over HTTP: sign parts, PUT them to their
// URLs on the public URL, and complete.
func (r *Runner) upload(ctx context.Context, testID string, subtitles bool) (string, error) {
	q := db.New(r.pool)
	app, err := q.GetBuiltinApp(ctx, r.projectID)
	if err != nil {
		return "", err
	}
	key := apikeys.Key{ProjectID: r.projectID, AppID: app.ID, Scopes: []string{apikeys.UploadsWrite}, Owner: true}
	up, err := r.uploads.Create(ctx, key, uploads.CreateRequest{
		Size: int64(len(sample)), Settings: ingest.Settings{
			PlaybackPolicies: []string{"signed"}, Passthrough: Passthrough, Generate: tracks.Generate{Subtitles: subtitles},
		},
	}, "")
	if err != nil {
		return "", fmt.Errorf("create upload: %w", err)
	}
	if err := q.SetSetupTestAsset(ctx, db.SetSetupTestAssetParams{ID: testID, AssetID: pgtype.Text{String: up.AssetID, Valid: true}}); err != nil {
		return "", err
	}
	api := r.urls.PublicURL() + "/v1/uploads/" + up.UploadID
	numbers := make([]int, up.PartCount)
	for i := range numbers {
		numbers[i] = i + 1
	}
	var signed uploads.Signed
	if err := r.postJSON(ctx, api+"/parts", up.UploadToken, map[string]any{"part_numbers": numbers}, &signed); err != nil {
		return "", fmt.Errorf("sign parts through the public URL: %w", err)
	}
	type part struct {
		Number int    `json:"part_number"`
		ETag   string `json:"etag"`
	}
	parts := make([]part, 0, len(signed.Parts))
	for _, p := range signed.Parts {
		start := int64(p.PartNumber-1) * up.PartSize
		end := min(start+up.PartSize, int64(len(sample)))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, p.URL, bytes.NewReader(sample[start:end]))
		res, err := r.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("upload a part to its signed URL: %s", errorKind(err))
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return "", fmt.Errorf("upload a part to its signed URL: HTTP %d", res.StatusCode)
		}
		parts = append(parts, part{Number: p.PartNumber, ETag: res.Header.Get("ETag")})
	}
	if err := r.postJSON(ctx, api+"/complete", up.UploadToken, map[string]any{"parts": parts}, nil); err != nil {
		return "", fmt.Errorf("complete through the public URL: %w", err)
	}
	return up.AssetID, nil
}

func (r *Runner) postJSON(ctx context.Context, u, bearer string, body, out any) error {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := r.client.Do(req)
	if err != nil {
		return errors.New(errorKind(err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); err != nil {
		return errors.New("the answer isn't ClipMux's API; is the public URL pointing at another server?")
	}
	return nil
}

// shouldSelfDial reports whether addr names a host served by this server's
// proxy (public_url, or media_url while built-in delivery is active). External
// hosts such as Cloudflare Workers and R2/S3 endpoints dial normally.
func shouldSelfDial(ctx context.Context, pool *pgxpool.Pool, urls *settings.Store, addr string) bool {
	if urls == nil {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if pub := urlHost(urls.PublicURL()); pub != "" && strings.EqualFold(host, pub) {
		return true
	}
	med := urlHost(urls.MediaURL())
	if med == "" || !strings.EqualFold(host, med) {
		return false
	}
	if pool == nil {
		return true
	}
	p, err := db.New(pool).GetProjectDelivery(ctx, db.DefaultProjectID)
	return err == nil && p.Delivery != "worker"
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// errorKind describes a transport error without the URL it carries.
func errorKind(err error) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		return "the hostname doesn't resolve"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "can't connect"
	case strings.Contains(err.Error(), "certificate"), strings.Contains(err.Error(), "tls"):
		return "TLS failed"
	}
	return "the request failed"
}

func (r *Runner) waitReady(parent context.Context, assetID string) error {
	ctx, cancel := context.WithTimeout(parent, r.EncodeTimeout)
	defer cancel()
	// A server shutting down cancels parent; that isn't a slow worker.
	timedOut := func() error {
		if err := parent.Err(); err != nil {
			return fmt.Errorf("stopped before the sample was ready: %w", err)
		}
		return fmt.Errorf("not ready after %s; is a worker running?", r.EncodeTimeout)
	}
	q := db.New(r.pool)
	for {
		a, err := q.GetAssetByID(ctx, assetID)
		switch {
		case err != nil && ctx.Err() != nil:
			return timedOut()
		case err != nil:
			return err
		case a.Status == "ready":
			return nil
		case a.Status == "errored":
			return fmt.Errorf("the sample failed to encode: %s", a.ErrorCode.String)
		}
		// A provider's machine that ended without claiming the sample fails
		// the test now rather than at the timeout. A failed wake may be a
		// moment without capacity, which the router retries; three in a
		// row (when the provider backs off) fail it (D097).
		if ds, err := q.RecentDispatchesForAsset(ctx, assetID); err == nil && len(ds) > 0 {
			unclaimed := func(d db.Dispatch) bool { return d.Status == "ended" && !d.AttemptID.Valid }
			d := ds[0]
			if unclaimed(d) && d.EndReason.String != dispatch.EndWakeFailed {
				return fmt.Errorf("the %s machine never took the sample: %s", d.Kind, d.EndReason.String)
			}
			failed := 0
			for _, x := range ds {
				if unclaimed(x) && x.EndReason.String == dispatch.EndWakeFailed {
					failed++
				}
			}
			if failed == 3 {
				return fmt.Errorf("the %s machine never took the sample: %s, 3 times in a row", d.Kind, d.EndReason.String)
			}
		}
		select {
		case <-ctx.Done():
			return timedOut()
		case <-time.After(r.Poll):
		}
	}
}

// playbackChecks plays a ready signed asset through the media URL.
func (r *Runner) playbackChecks(ctx context.Context, assetID string) []Check {
	rs := &results{}
	pid, signedBase, thumbBase, err := r.bases(ctx, assetID)
	if err != nil {
		rs.add("play_master", err)
		rs.skip(PlaybackChecks[1:]...)
		return rs.checks
	}
	media := r.urls.MediaURL()

	// The master names a video playlist; that names the key and a segment.
	var keyRel, segRel string
	var iv []byte
	status, master, err := r.get(ctx, signedBase+"master.m3u8")
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	if err == nil {
		var playlistRel string
		if playlistRel, err = firstVariant(string(master)); err == nil {
			var body []byte
			if status, body, err = r.get(ctx, signedBase+playlistRel); err == nil && status != http.StatusOK {
				err = fmt.Errorf("media playlist: HTTP %d", status)
			}
			if err == nil {
				keyRel, segRel, iv, err = keyAndSegment(playlistRel, string(body))
			}
		}
	}
	if !rs.add("play_master", err) {
		rs.skip(PlaybackChecks[1:]...)
		return rs.checks
	}
	status, key, err := r.get(ctx, signedBase+keyRel)
	switch {
	case err == nil && status != http.StatusOK:
		err = fmt.Errorf("HTTP %d", status)
	case err == nil && len(key) != 16:
		err = fmt.Errorf("the key is %d bytes, want 16", len(key))
	}
	if !rs.add("play_key", err) {
		rs.skip(PlaybackChecks[2:]...)
		return rs.checks
	}
	status, seg, err := r.get(ctx, signedBase+segRel)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	if err == nil && !fragmentAfterDecrypt(key, iv, seg) {
		err = errors.New("the segment doesn't decrypt to an fMP4 fragment with the delivered key")
	}
	rs.add("play_segment_decrypts", err)
	rs.failed = "" // the refusal checks don't depend on decryption
	rs.add("thumbnail_token_refused_segment", wantStatus(r.get(ctx, thumbBase+segRel))(http.StatusForbidden))
	rs.add("thumbnail_token_refused_key", wantStatus(r.get(ctx, thumbBase+keyRel))(http.StatusForbidden))
	rs.add("thumbnail_token_poster", wantStatus(r.get(ctx, thumbBase+"poster.jpg"))(http.StatusOK))
	rs.add("no_token_refused", wantStatus(r.get(ctx, media+"/p/"+pid+"/master.m3u8"))(http.StatusForbidden))
	rs.add("stored_segment_encrypted", r.storedEncrypted(ctx, assetID, segRel))
	return rs.checks
}

// bases are the sample's signed URL prefixes on the media URL: one with a
// v token, one with a t token.
func (r *Runner) bases(ctx context.Context, assetID string) (pid, signed, thumb string, err error) {
	pid, err = r.signedPlaybackID(ctx, assetID)
	if err != nil {
		return "", "", "", err
	}
	signer, err := signingkeys.Internal(ctx, r.pool, r.box, r.projectID)
	if err != nil {
		return "", "", "", err
	}
	now := time.Now().Unix()
	v, _ := signer.Sign(tokens.Claims{Sub: pid, Aud: tokens.AudiencePlayback, Iat: now, Exp: now + 600})
	t, _ := signer.Sign(tokens.Claims{Sub: pid, Aud: tokens.AudienceThumbnail, Iat: now, Exp: now + 600})
	media := r.urls.MediaURL()
	return pid, media + "/v/" + v + "/" + pid + "/", media + "/v/" + t + "/" + pid + "/", nil
}

// trackChecks waits for the sample's subtitles and plays them through the
// media URL: listed in the master, a cue with the sample's word at its
// time, refused to a t token, and ciphertext in storage.
func (r *Runner) trackChecks(ctx context.Context, assetID string) []Check {
	rs := &results{}
	trackID, err := r.waitSubtitles(ctx, assetID)
	if !rs.add("subtitles_ready", err) {
		rs.skip(TrackChecks[1:]...)
		return rs.checks
	}
	_, signedBase, thumbBase, err := r.bases(ctx, assetID)
	if err == nil {
		err = r.playSubtitles(ctx, signedBase, trackID)
	}
	rs.add("play_subtitles", err)
	rs.failed = "" // the refusal and storage checks stand on their own
	rs.add("thumbnail_token_refused_track", wantStatus(r.get(ctx, thumbBase+"tracks/"+trackID+".vtt"))(http.StatusForbidden))
	rs.add("stored_track_encrypted", r.storedTrackEncrypted(ctx, assetID, trackID))
	return rs.checks
}

// waitSubtitles waits for the sample's subtitles track to be ready.
func (r *Runner) waitSubtitles(parent context.Context, assetID string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, r.EncodeTimeout)
	defer cancel()
	q := db.New(r.pool)
	for {
		ts, err := q.ListTracksForAsset(ctx, assetID)
		if err != nil && ctx.Err() == nil {
			return "", err
		}
		for _, t := range ts {
			switch {
			case t.Kind != tracks.Subtitles:
			case t.Status == tracks.Ready:
				return t.ID, nil
			case t.Status == tracks.Errored:
				return "", fmt.Errorf("the transcription failed: %s", t.ErrorCode.String)
			}
		}
		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				return "", fmt.Errorf("stopped before the subtitles were ready: %w", err)
			}
			return "", fmt.Errorf("no subtitles after %s; is a worker with speech running?", r.EncodeTimeout)
		case <-time.After(r.Poll):
		}
	}
}

// playSubtitles follows the master to the subtitles playlist and its VTT,
// and finds the sample's word in a cue at its time.
func (r *Runner) playSubtitles(ctx context.Context, signedBase, trackID string) error {
	status, master, err := r.get(ctx, signedBase+"master.m3u8")
	if err != nil {
		return err
	}
	playlist := "tracks/" + trackID + ".m3u8"
	if status != http.StatusOK || !strings.Contains(string(master), `TYPE=SUBTITLES`) || !strings.Contains(string(master), `URI="`+playlist+`"`) {
		return fmt.Errorf("the master (HTTP %d) doesn't list the subtitles", status)
	}
	status, body, err := r.get(ctx, signedBase+playlist)
	if err == nil && (status != http.StatusOK || !strings.Contains(string(body), trackID+".vtt")) {
		err = fmt.Errorf("subtitles playlist: HTTP %d", status)
	}
	if err != nil {
		return err
	}
	status, vtt, err := r.get(ctx, signedBase+"tracks/"+trackID+".vtt")
	if err == nil && (status != http.StatusOK || !bytes.HasPrefix(vtt, []byte("WEBVTT"))) {
		err = fmt.Errorf("subtitles VTT: HTTP %d", status)
	}
	if err != nil {
		return err
	}
	for _, c := range cues(string(vtt)) {
		if strings.Contains(strings.ToLower(c.Text), expectedWord) && c.StartMS <= expectedAtMS+cueSlackMS && c.EndMS >= expectedAtMS-cueSlackMS {
			return nil
		}
	}
	return fmt.Errorf("no cue says %q at %.1f s ± %.1f s: the subtitles are out of time or not of the sample's speech", expectedWord, float64(expectedAtMS)/1000, float64(cueSlackMS)/1000)
}

// cues reads the cues of a WebVTT file serve wrote.
func cues(vtt string) []transcripts.Cue {
	var out []transcripts.Cue
	lines := strings.Split(vtt, "\n")
	for i, l := range lines {
		from, to, ok := strings.Cut(l, " --> ")
		if !ok || i+1 >= len(lines) {
			continue
		}
		start, err1 := parseVTTTime(from)
		end, err2 := parseVTTTime(to)
		if err1 == nil && err2 == nil {
			out = append(out, transcripts.Cue{StartMS: start, EndMS: end, Text: lines[i+1]})
		}
	}
	return out
}

func parseVTTTime(s string) (int64, error) {
	var h, m, sec, ms int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d:%d.%d", &h, &m, &sec, &ms); err != nil {
		return 0, err
	}
	return ((h*60+m)*60+sec)*1000 + ms, nil
}

// storedTrackEncrypted reads the subtitles straight from storage: they must
// not be WebVTT.
func (r *Runner) storedTrackEncrypted(ctx context.Context, assetID, trackID string) error {
	store, err := r.stores.MediaFor(ctx, assetID)
	if err != nil {
		return err
	}
	rc, err := store.Get(ctx, tracks.StorageKey(assetID, trackID))
	if err != nil {
		return fmt.Errorf("read the stored subtitles: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return err
	}
	if bytes.Contains(body, []byte("WEBVTT")) || bytes.Contains(bytes.ToLower(body), []byte(expectedWord)) {
		return errors.New("the stored subtitles are plain WebVTT, not ciphertext")
	}
	return nil
}

func wantStatus(status int, _ []byte, err error) func(int) error {
	return func(want int) error {
		if err != nil {
			return err
		}
		if status != want {
			return fmt.Errorf("HTTP %d, want %d", status, want)
		}
		return nil
	}
}

func (r *Runner) signedPlaybackID(ctx context.Context, assetID string) (string, error) {
	pids, err := db.New(r.pool).ListPlaybackIDs(ctx, assetID)
	if err != nil {
		return "", err
	}
	for _, p := range pids {
		if p.Policy == "signed" && p.Status == "active" {
			return p.ID, nil
		}
	}
	return "", errors.New("the sample has no active signed playback ID")
}

func (r *Runner) get(ctx context.Context, u string) (int, []byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	res, err := r.client.Do(req)
	if err != nil {
		return 0, nil, errors.New(errorKind(err))
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	return res.StatusCode, body, err
}

// firstVariant returns the first variant playlist URI of a master.
func firstVariant(master string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(master, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#EXT-X-STREAM-INF") && i+1 < len(lines) && lines[i+1] != "" {
			return strings.TrimSpace(lines[i+1]), nil
		}
	}
	return "", errors.New("the master lists no variant")
}

// keyAndSegment reads a media playlist at playlistRel (relative to the
// playback ID) and returns its key URI, first segment and IV, each relative
// to the playback ID.
func keyAndSegment(playlistRel, body string) (string, string, []byte, error) {
	base, _ := url.Parse("https://x/" + playlistRel)
	resolve := func(ref string) string {
		u, err := base.Parse(ref)
		if err != nil {
			return ""
		}
		return strings.TrimPrefix(u.Path, "/")
	}
	var keyURI, seg string
	var iv []byte
	seq := 0
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.Atoi(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"))
		case strings.HasPrefix(l, "#EXT-X-KEY:"):
			attrs := l[len("#EXT-X-KEY:"):]
			if u := attr(attrs, "URI"); u != "" {
				keyURI = resolve(strings.Trim(u, `"`))
			}
			if v := attr(attrs, "IV"); v != "" {
				iv, _ = hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X"))
			}
		case l != "" && !strings.HasPrefix(l, "#") && seg == "":
			seg = resolve(strings.TrimSpace(l))
		}
	}
	if keyURI == "" || seg == "" {
		return "", "", nil, errors.New("the media playlist names no key or no segment")
	}
	if len(iv) != 16 {
		// Without an IV, HLS uses the segment's media sequence number.
		iv = make([]byte, 16)
		for i, s := 15, seq; i >= 8; i, s = i-1, s>>8 {
			iv[i] = byte(s)
		}
	}
	return keyURI, seg, iv, nil
}

// attr finds NAME=value in an HLS attribute list.
func attr(list, name string) string {
	for len(list) > 0 {
		k, rest, _ := strings.Cut(list, "=")
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				return ""
			}
			v, rest = rest[:end+2], rest[end+2:]
		} else {
			v, rest, _ = strings.Cut(rest, ",")
			rest = "," + rest
		}
		if strings.TrimSpace(k) == name {
			return v
		}
		list = strings.TrimPrefix(rest, ",")
	}
	return ""
}

// fragmentAfterDecrypt reports whether AES-128-CBC with key and iv turns seg
// into an fMP4 fragment.
func fragmentAfterDecrypt(key, iv, seg []byte) bool {
	if len(key) != 16 || len(iv) != 16 {
		return false
	}
	plain, err := media.DecryptSegment([16]byte(key), [16]byte(iv), seg)
	return err == nil && isFragment(plain)
}

// isFragment walks the leading top-level MP4 boxes (styp, sidx, …) and
// reports whether a moof follows them. Ciphertext fails at the first box.
func isFragment(b []byte) bool {
	for off, i := 0, 0; off+8 <= len(b) && i < 8; i++ {
		size := int(binary.BigEndian.Uint32(b[off:]))
		if string(b[off+4:off+8]) == "moof" {
			return true
		}
		if size < 8 || size > len(b)-off {
			return false
		}
		off += size
	}
	return false
}

// storedEncrypted reads the segment straight from storage: it must not be a
// plain MP4 fragment.
func (r *Runner) storedEncrypted(ctx context.Context, assetID, segRel string) error {
	// segRel is {output_id}/{rendition}/seg_…; storage keeps it under the asset.
	store, err := r.stores.MediaFor(ctx, assetID)
	if err != nil {
		return err
	}
	rc, err := store.Get(ctx, "media/"+assetID+"/"+segRel)
	if err != nil {
		return fmt.Errorf("read the stored segment: %w", err)
	}
	defer rc.Close()
	head := make([]byte, 4096)
	n, _ := io.ReadFull(rc, head)
	if isFragment(head[:n]) {
		return errors.New("the stored segment is a plain fMP4 fragment, not ciphertext")
	}
	return nil
}
