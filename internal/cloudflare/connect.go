package cloudflare

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/fence"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// workerJS is the bundled Worker (worker/, `make worker`); CI checks it is
// current.
//
//go:embed worker.js
var workerJS []byte

// WorkerVersion identifies the embedded Worker build.
func WorkerVersion() string {
	sum := sha256.Sum256(workerJS)
	return hex.EncodeToString(sum[:6])
}

// CompatibilityDate is the Workers runtime behaviour the Worker is tested
// against (worker/src/workerd.test.ts).
const CompatibilityDate = "2026-07-01"

// DefaultBucket is the R2 bucket Connect creates when the owner names none.
const DefaultBucket = "clipmux-media"

// ScriptFor names the Worker after its media hostname, so installs that
// share a Cloudflare account (production and staging, say) each deploy
// their own Worker instead of overwriting one. Cloudflare caps script names
// at 63 characters: a longer name keeps its start and a hash of the
// hostname, so two long hostnames still differ.
func ScriptFor(hostname string) string {
	name := "clipmux-" + strings.ReplaceAll(hostname, ".", "-")
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(hostname))
	return strings.TrimRight(name[:54], "-") + "-" + hex.EncodeToString(sum[:4])
}

// AnalyticsDataset is the Analytics Engine dataset the Worker writes to.
// Installs sharing an account share it; each data point names its project.
const AnalyticsDataset = "clipmux_delivery"

// Request is what the owner gives: a token, the media hostname, and
// optionally the bucket (create it first to scope the token to it) and the
// account when the token reaches several.
type Request struct {
	Token     string `json:"token"`
	Hostname  string `json:"hostname"`
	Bucket    string `json:"bucket,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// R2Request adds an R2 bucket as storage without moving delivery (D110): a
// token that can edit R2, the bucket (created if missing), the account when
// the token reaches several, and whether new uploads go there.
type R2Request struct {
	Token     string `json:"token"`
	Bucket    string `json:"bucket,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Default   bool   `json:"default,omitempty"`
}

// Step is one stage of connecting, reported to the dashboard.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Result lists every step taken; Connected is true when all passed.
type Result struct {
	Connected bool   `json:"connected"`
	Steps     []Step `json:"steps"`
}

// Connector connects a project to Cloudflare.
type Connector struct {
	Pool      *pgxpool.Pool
	Stores    *stores.Set
	Keys      contentkey.Ring
	URLs      *settings.Store
	ProjectID string
	// AnalyticsKey hashes viewer IDs (analytics.ViewerKey); the Worker gets
	// it as a secret so edge and origin viewer keys agree.
	AnalyticsKey []byte
	// API is Cloudflare's API base, and R2Endpoint an account's S3
	// endpoint; tests point both at fakes.
	API        string
	R2Endpoint func(accountID string) string
	HTTP       *http.Client
	// SetupTest queues the setup test once delivery has moved.
	SetupTest func(ctx context.Context) error
}

// r2Endpoint is an account's R2 S3 API.
func r2Endpoint(accountID string) string { return "https://" + accountID + ".r2.cloudflarestorage.com" }

// EndpointTemplate turns "https://{account_id}.eu.r2.cloudflarestorage.com"
// into an endpoint function; empty means the default.
func EndpointTemplate(tmpl string) func(string) string {
	if tmpl == "" {
		return nil
	}
	return func(accountID string) string { return strings.ReplaceAll(tmpl, "{account_id}", accountID) }
}

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// ErrInvalid is a request the owner must correct.
var ErrInvalid = errors.New("cloudflare: invalid request")

// Connect runs every step in order and stops at the first that fails. Each
// step is safe to repeat, so the owner fixes the cause and connects again;
// a later connect with a new token is also how an upgraded Worker is
// redeployed.
func (c *Connector) Connect(ctx context.Context, req Request) (Result, error) {
	req.Hostname = strings.ToLower(strings.TrimSpace(req.Hostname))
	if req.Bucket == "" {
		req.Bucket = DefaultBucket
	}
	switch {
	case req.Token == "":
		return Result{}, fmt.Errorf("%w: a token is required", ErrInvalid)
	case !hostnameRE.MatchString(req.Hostname):
		return Result{}, fmt.Errorf("%w: %q is not a hostname", ErrInvalid, req.Hostname)
	case !bucketRE.MatchString(req.Bucket):
		return Result{}, fmt.Errorf("%w: %q is not an R2 bucket name", ErrInvalid, req.Bucket)
	}
	if pub, err := url.Parse(c.URLs.PublicURL()); err == nil && pub.Hostname() == req.Hostname {
		return Result{}, fmt.Errorf("%w: the media hostname must differ from the public URL's", ErrInvalid)
	}

	q := db.New(c.Pool)
	prev, err := q.GetCloudflareConnection(ctx, c.ProjectID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("cloudflare connection: %w", err)
	}
	connected := err == nil
	// A reconnect to the same hostname redeploys the Worker it made before,
	// whatever that one is called (installs connected before Workers were
	// named per hostname share one fixed name).
	script := ScriptFor(req.Hostname)
	if connected && prev.Hostname == req.Hostname {
		script = prev.ScriptName
	}

	cf := c.client(req.Token)
	var res Result
	step := stepper(&res.Steps)
	tokenID, accountID, ok := credential(ctx, cf, req.AccountID, step)
	var zoneID, storageID, installID string
	ok = ok && step("zone", func() (string, error) {
		z, err := cf.ZoneFor(ctx, accountID, req.Hostname)
		if err != nil {
			return "", err
		}
		if z.Status != "active" {
			return "", fmt.Errorf("zone %s is %s; it must be active in this account", z.Name, z.Status)
		}
		zoneID = z.ID
		return z.Name, nil
	}) && step("workers_paid", func() (string, error) {
		paid, err := cf.WorkersPaid(ctx, accountID)
		if err == nil && !paid {
			err = errors.New("the account needs Workers Paid: video delivery exceeds the free plan's limits and terms")
		}
		return "subscribed", err
	}) && c.r2Store(ctx, cf, req.Token, tokenID, accountID, req.Bucket, step, &storageID) && step("worker", func() (string, error) {
		installID, err = q.GetAnalyticsInstallID(ctx, c.ProjectID)
		if err != nil {
			return "", err
		}
		return "version " + WorkerVersion(), cf.UploadWorker(ctx, accountID, script, workerJS, CompatibilityDate, []Binding{
			{Type: "r2_bucket", Name: "MEDIA", BucketName: req.Bucket},
			{Type: "plain_text", Name: "ORIGIN_URL", Text: c.URLs.PublicURL()},
			{Type: "secret_text", Name: "CONTENT_KEYS", Text: c.Keys.WorkerSecret()},
			{Type: "analytics_engine", Name: "ANALYTICS", Dataset: AnalyticsDataset},
			{Type: "plain_text", Name: "PROJECT_ID", Text: c.ProjectID},
			{Type: "plain_text", Name: "INSTALL_ID", Text: installID},
			{Type: "secret_text", Name: "ANALYTICS_KEY", Text: hex.EncodeToString(c.AnalyticsKey)},
		})
	}) && step("routes", func() (string, error) {
		// The bucket gets every route before the hostname moves, so the
		// Worker serves from its first request. New videos' encoded files go
		// to the bucket (D117); uploads stay where Storage sends them, and
		// videos encoded elsewhere play through origin until they move.
		if err := q.SetProjectDelivery(ctx, db.SetProjectDeliveryParams{ID: c.ProjectID, Delivery: "worker", DeliveryStorageID: pgtype.Text{String: storageID, Valid: true}}); err != nil {
			return "", err
		}
		d, bucketID, err := routes.ForProject(ctx, q, c.Stores, c.ProjectID)
		if err != nil {
			return "", err
		}
		// A bucket another install published into, or this one before its
		// database was restored, keeps what this database doesn't know: the
		// fence goes up before the first reconcile could delete it (D144).
		g, err := fence.NewGuard(ctx, q, c.ProjectID, nil)
		if err != nil {
			return "", err
		}
		fenced, err := g.CheckBucket(ctx, bucketID, d.Bucket())
		if err != nil {
			return "", err
		}
		r, err := routes.Reconcile(ctx, q, d, bucketID, c.ProjectID)
		msg := fmt.Sprintf("%d routes written; new videos are encoded into the bucket", r.Written)
		if fenced {
			msg += fmt.Sprintf("; the restore fence is up, so %d route records this database doesn't know were kept: read `clipmux admin restore report`, then `clipmux admin restore finish --yes`", r.Kept)
		}
		return msg, err
	}) && step("dns", func() (string, error) {
		attached, err := cf.DomainMatches(ctx, accountID, zoneID, req.Hostname, script)
		if err != nil {
			return "", err
		}
		if attached {
			return "already attached to this Worker; managed DNS preserved", nil
		}
		n, err := cf.ClearHostname(ctx, zoneID, req.Hostname)
		return fmt.Sprintf("%d records at %s removed", n, req.Hostname), err
	}) && step("domain", func() (string, error) {
		return req.Hostname, cf.AttachDomain(ctx, accountID, zoneID, req.Hostname, script)
	}) && step("media_url", func() (string, error) {
		want := "https://" + req.Hostname
		if c.URLs.URLs().Pinned {
			// The environment owns the URLs; it must already name the Worker.
			if c.URLs.MediaURL() != want {
				return "", fmt.Errorf("CLIPMUX_MEDIA_URL pins the media URL to %s; set it to %s and restart", c.URLs.MediaURL(), want)
			}
			return want + " (pinned)", nil
		}
		u, err := c.URLs.Set(ctx, c.URLs.PublicURL(), want)
		return u.Media, err
	}) && step("saved", func() (string, error) {
		_, err := q.SaveCloudflareConnection(ctx, db.SaveCloudflareConnectionParams{
			ProjectID: c.ProjectID, AccountID: accountID, ZoneID: zoneID, Hostname: req.Hostname, Bucket: req.Bucket,
			ScriptName: script, TokenID: tokenID, StorageID: storageID, WorkerVersion: WorkerVersion(),
		})
		return "the token itself is not kept; its Workers and DNS permissions stay live in Cloudflare", err
	})
	if ok && c.SetupTest != nil {
		ok = step("setup_test", func() (string, error) { return "queued", c.SetupTest(ctx) })
	}
	res.Connected = ok
	return res, nil
}

// AddR2 saves an R2 bucket as storage, creating it if missing, without
// touching delivery (D110). It returns the storage config's ID; adding the
// same bucket again renews its credentials, and Connect reuses it.
func (c *Connector) AddR2(ctx context.Context, req R2Request) (Result, string, error) {
	if req.Bucket == "" {
		req.Bucket = DefaultBucket
	}
	switch {
	case req.Token == "":
		return Result{}, "", fmt.Errorf("%w: a token is required", ErrInvalid)
	case !bucketRE.MatchString(req.Bucket):
		return Result{}, "", fmt.Errorf("%w: %q is not an R2 bucket name", ErrInvalid, req.Bucket)
	}
	cf := c.client(req.Token)
	var res Result
	step := stepper(&res.Steps)
	var storageID string
	tokenID, accountID, ok := credential(ctx, cf, req.AccountID, step)
	ok = ok && c.r2Store(ctx, cf, req.Token, tokenID, accountID, req.Bucket, step, &storageID)
	if ok && req.Default {
		ok = step("default", func() (string, error) {
			return "new uploads go to " + req.Bucket, c.Stores.SetDefault(ctx, c.ProjectID, storageID)
		})
	}
	res.Connected = ok
	return res, storageID, nil
}

// stepFunc runs one named step and records its result.
type stepFunc func(name string, fn func() (string, error)) bool

// stepper appends each step's result to dst and reports whether it passed.
// A failed step's error is its detail.
func stepper(dst *[]Step) stepFunc {
	return func(name string, fn func() (string, error)) bool {
		detail, err := fn()
		s := Step{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			s.Detail = err.Error()
		}
		*dst = append(*dst, s)
		return err == nil
	}
}

func (c *Connector) client(token string) *Client {
	return &Client{Base: c.API, Token: token, HTTP: c.HTTP}
}

// credential is the token and account steps every flow starts with: the
// token is active, and it names one account (or the one asked for).
func credential(ctx context.Context, cf *Client, wantAccount string, step stepFunc) (tokenID, accountID string, ok bool) {
	ok = step("token", func() (string, error) {
		var err error
		tokenID, err = cf.VerifyToken(ctx)
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
			// Cloudflare rejects account-owned tokens at the user endpoint.
			// Discover the account with the same credential, then verify there.
			account := wantAccount
			if account == "" {
				accounts, listErr := cf.Accounts(ctx)
				if listErr != nil {
					return "", listErr
				}
				if len(accounts) != 1 {
					return "", fmt.Errorf("account-owned tokens require account_id when several accounts are accessible")
				}
				account = accounts[0].ID
			}
			tokenID, err = cf.VerifyAccountToken(ctx, account)
		}
		return "active", err
	}) && step("account", func() (string, error) {
		accts, err := cf.Accounts(ctx)
		if err != nil {
			return "", err
		}
		for _, a := range accts {
			if a.ID == wantAccount || (wantAccount == "" && len(accts) == 1) {
				accountID = a.ID
				return a.Name, nil
			}
		}
		names := make([]string, len(accts))
		for i, a := range accts {
			names[i] = a.Name + " (" + a.ID + ")"
		}
		return "", fmt.Errorf("choose the account: %s", strings.Join(names, ", "))
	})
	return tokenID, accountID, ok
}

// r2Store is the bucket and storage steps: the bucket exists, and a storage
// config reaches it with keys derived from the token. It sets *storageID.
func (c *Connector) r2Store(ctx context.Context, cf *Client, token, tokenID, accountID, bucket string, step stepFunc, storageID *string) bool {
	q := db.New(c.Pool)
	return step("bucket", func() (string, error) {
		created, err := cf.EnsureBucket(ctx, accountID, bucket)
		if created {
			return bucket + " created; unless the token is scoped to one bucket, it reaches every bucket in the account", err
		}
		return bucket + " exists", err
	}) && step("storage", func() (string, error) {
		// R2's S3 credentials derive from the token: its ID and the
		// SHA-256 of its value. They stop working if the token is revoked.
		sum := sha256.Sum256([]byte(token))
		cfg := stores.S3Config{Endpoint: c.endpoint(accountID), Region: "auto", Bucket: bucket, AccountID: accountID}
		sec := stores.S3Secret{AccessKeyID: tokenID, SecretAccessKey: hex.EncodeToString(sum[:])}
		existing, err := q.FindStorageConfigByBucket(ctx, db.FindStorageConfigByBucketParams{ProjectID: c.ProjectID, Bucket: bucket, AccountID: accountID})
		switch {
		case err == nil:
			*storageID = existing.ID
			return "credentials renewed", c.Stores.UpdateS3Secret(ctx, existing.ID, cfg, sec)
		case !errors.Is(err, pgx.ErrNoRows):
			return "", err
		}
		row, err := c.Stores.AddS3(ctx, c.ProjectID, stores.KindR2, cfg, sec)
		*storageID = row.ID
		return "R2 bucket checked: list, write, read, delete and CORS", err
	})
}

// ErrNotConnected means there is no Cloudflare connection to undo.
var ErrNotConnected = errors.New("cloudflare: not connected")

// ErrMediaURLPinned means CLIPMUX_MEDIA_URL names the Worker's hostname, so
// delivery can't move back while the environment says so.
var ErrMediaURLPinned = errors.New("cloudflare: the environment pins the media URL to the Worker")

// Disconnected is Disconnect's report, step by step like Connect's.
type Disconnected struct {
	Disconnected bool   `json:"disconnected"`
	Steps        []Step `json:"steps"`
}

// Disconnect returns the project to built-in delivery (D099). The R2 store
// stays a storage config, still the default if it was: its assets play
// through this server. Steps stop at the first failure, and each is safe
// to repeat:
//
//   - media_url: back to the public URL, so new playback URLs reach origin;
//   - delivery: the built-in handler;
//   - routes: the Worker's route records and _keys.json leave the bucket, so
//     a request that still reaches the Worker is refused rather than served
//     from records no one updates any more;
//   - forgotten: the connection row goes.
//
// The Worker, its custom domain and the token stay in the Cloudflare
// account, since no token is kept to remove them.
func (c *Connector) Disconnect(ctx context.Context) (Disconnected, error) {
	q := db.New(c.Pool)
	conn, err := q.GetCloudflareConnection(ctx, c.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Disconnected{}, ErrNotConnected
	}
	if err != nil {
		return Disconnected{}, fmt.Errorf("cloudflare connection: %w", err)
	}
	worker := "https://" + conn.Hostname
	if c.URLs.URLs().Pinned && c.URLs.MediaURL() == worker {
		return Disconnected{}, ErrMediaURLPinned
	}
	var res Disconnected
	step := stepper(&res.Steps)
	ok := step("media_url", func() (string, error) {
		if c.URLs.MediaURL() != worker {
			return c.URLs.MediaURL() + " (unchanged)", nil
		}
		u, err := c.URLs.Set(ctx, c.URLs.PublicURL(), "")
		return u.Media, err
	}) && step("delivery", func() (string, error) {
		return "built-in", q.SetProjectDelivery(ctx, db.SetProjectDeliveryParams{ID: c.ProjectID, Delivery: "builtin"})
	}) && step("routes", func() (string, error) {
		bucket, err := c.Stores.Get(ctx, conn.StorageID)
		if err != nil {
			return "", err
		}
		if err := bucket.DeletePrefix(ctx, "_routes/"); err != nil {
			return "", fmt.Errorf("remove the Worker's routes (fix the R2 credentials, or remove the Worker in Cloudflare): %w", err)
		}
		if err := bucket.Delete(ctx, routes.KeysKey); err != nil {
			return "", fmt.Errorf("remove the Worker's keys: %w", err)
		}
		return "the Worker refuses every playback ID from now on", nil
	}) && step("forgotten", func() (string, error) {
		_, err := q.DeleteCloudflareConnection(ctx, c.ProjectID)
		return "the Worker " + conn.ScriptName + " and " + conn.Hostname + " stay in your Cloudflare account; remove them there", err
	})
	if ok && c.SetupTest != nil {
		ok = step("setup_test", func() (string, error) { return "queued", c.SetupTest(ctx) })
	}
	res.Disconnected = ok
	return res, nil
}

func (c *Connector) endpoint(accountID string) string {
	if c.R2Endpoint != nil {
		return c.R2Endpoint(accountID)
	}
	return r2Endpoint(accountID)
}

// Status is what the dashboard shows about the connection.
type Status struct {
	Connected      bool   `json:"connected"`
	Hostname       string `json:"hostname,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	AccountID      string `json:"account_id,omitempty"`
	WorkerVersion  string `json:"worker_version,omitempty"`
	CurrentVersion string `json:"current_version"`
	// NeedsRedeploy: this binary carries a newer Worker; connecting again
	// with a token deploys it.
	NeedsRedeploy bool `json:"needs_redeploy"`
	// StorageOK is false when R2 refuses the stored credentials (the token
	// was revoked or rolled): the dashboard offers Reconnect Cloudflare.
	StorageOK bool `json:"storage_ok"`
	// Create-token links for Connect, R2 storage and edge analytics (D109).
	TokenURL          string `json:"token_url"`
	R2TokenURL        string `json:"r2_token_url"`
	AnalyticsTokenURL string `json:"analytics_token_url"`
}

// Status reads the connection and checks that R2 still accepts it.
func (c *Connector) Status(ctx context.Context) (Status, error) {
	st := Status{CurrentVersion: WorkerVersion(), TokenURL: ConnectTokenURL(), R2TokenURL: R2TokenURL(), AnalyticsTokenURL: AnalyticsTokenURL()}
	row, err := db.New(c.Pool).GetCloudflareConnection(ctx, c.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Connected, st.Hostname, st.Bucket, st.AccountID, st.WorkerVersion = true, row.Hostname, row.Bucket, row.AccountID, row.WorkerVersion
	st.NeedsRedeploy = row.WorkerVersion != WorkerVersion()
	if store, err := c.Stores.Get(ctx, row.StorageID); err == nil {
		_, _, err = store.List(ctx, "_routes/", "")
		st.StorageOK = err == nil
	}
	return st, nil
}
