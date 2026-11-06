// Package worker is the executor side of the worker API
// (docs/architecture.md, "Compute"): claim a job, heartbeat while working,
// download the source, transcode and package it, upload the output through
// signed URLs, then complete or fail the attempt. The same code runs as the
// built-in worker, a PC agent and a serverless worker; it never holds a
// storage credential, only an attempt token.
package worker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// Worker claims and runs jobs against one ClipMux server.
type Worker struct {
	// Server is the ClipMux base URL, e.g. http://127.0.0.1:8080.
	Server string
	// Key authenticates claims.
	Key string
	// WorkDir holds one directory per attempt, removed when it ends.
	WorkDir string
	Tools   media.Tools
	HTTP    *http.Client
	Logger  *slog.Logger
	// Wait is the claim long-poll; HeartbeatEvery overrides the server's
	// interval (tests).
	Wait           time.Duration
	HeartbeatEvery time.Duration
	// MaxJob bounds one attempt's wall-clock time (default 6 h). Without it a
	// wedged attempt would heartbeat its lease forever.
	MaxJob time.Duration
	// IdleTimeout fails a transfer that moves no bytes for this long
	// (default 60 s), so a half-open connection can't hang the attempt.
	IdleTimeout time.Duration
	// Sandbox describes what confines the tools, reported at claim
	// ("none" when nothing does).
	Sandbox string
	// StorageViaServer sends signed filesystem-storage URLs (/_storage/…)
	// to Server instead of the public URL they name: the built-in worker
	// reaches serve over loopback. The signature doesn't cover the host.
	StorageViaServer bool
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// storageURL applies StorageViaServer to a signed URL.
func (w *Worker) storageURL(raw string) string {
	if !w.StorageViaServer {
		return raw
	}
	u, err := neturl.Parse(raw)
	s, serr := neturl.Parse(w.Server)
	if err != nil || serr != nil || !strings.HasPrefix(u.Path, "/_storage/") {
		return raw
	}
	u.Scheme, u.Host = s.Scheme, s.Host
	return u.String()
}

var errStopped = errors.New("worker: the server told this attempt to stop")

// Failure codes the worker reports.
const (
	codeUnsupportedSource = "unsupported_source"
	codeDownload          = "source_download_failed"
	codeTranscode         = "transcode_failed"
	codeUpload            = "upload_failed"
	codeCommit            = "commit_failed"
	codeTimeout           = "job_timeout"
)

// ErrKeyRefused means the server no longer accepts this worker's key: an
// agent that was revoked. Retrying can't help, so Run returns it.
var ErrKeyRefused = errors.New("worker: the server refused this worker's key")

// Run claims and runs jobs until ctx ends, or until the server refuses the
// key. It starts by clearing attempt directories a crash left behind: only
// this worker uses WorkDir. Network errors back off from 1 s to 30 s, so a
// PC waking from sleep or regaining Wi-Fi is back within half a minute.
func (w *Worker) Run(ctx context.Context) error {
	if err := ClearDir(w.WorkDir); err != nil {
		w.Logger.Warn("clear the scratch directory", "dir", w.WorkDir, "err", err)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			if errors.Is(err, ErrKeyRefused) {
				return err
			}
			w.Logger.Error("worker", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
	}
	return nil
}

// ClearDir removes everything inside dir, keeping dir.
func ClearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// RunOnce claims at most one job and runs it. It reports whether it got one.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, err := w.claim(ctx)
	if err != nil || job == nil {
		return false, err
	}
	w.Logger.Info("claimed job", "job", job.JobID, "attempt", job.AttemptID, "kind", job.Kind)
	if err := w.handle(ctx, job); err != nil {
		return true, err
	}
	return true, nil
}

func (w *Worker) claim(ctx context.Context) (*workerapi.Job, error) {
	versions, err := w.Tools.Versions(ctx)
	if err != nil {
		return nil, fmt.Errorf("media tools: %w", err)
	}
	wait := w.Wait
	if wait == 0 {
		wait = 25 * time.Second
	}
	var job workerapi.Job
	status, err := w.post(ctx, "/worker/v1/claim", w.Key, map[string]any{
		"contract_versions": []int{workerapi.ContractVersion},
		"ffmpeg_version":    versions.FFmpeg,
		"packager_version":  versions.Packager,
		"encoders":          []string{"libx264"},
		"wait_seconds":      wait.Seconds(),
		"sandbox":           orNone(w.Sandbox),
		"free_disk_bytes":   freeDisk(w.WorkDir),
	}, &job)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusUnauthorized:
		return nil, ErrKeyRefused
	case status == http.StatusNoContent:
		return nil, nil
	case status != http.StatusOK:
		return nil, fmt.Errorf("claim: HTTP %d", status)
	}
	if !validID.MatchString(job.AttemptID) {
		return nil, fmt.Errorf("claim: malformed attempt id %q", job.AttemptID)
	}
	return &job, nil
}

var validID = regexp.MustCompile(`^att_[0-9a-z]{26}$`)

// handle runs one attempt to its end and reports the outcome.
func (w *Worker) handle(ctx context.Context, job *workerapi.Job) error {
	dir := filepath.Join(w.WorkDir, job.AttemptID)
	defer os.RemoveAll(dir)

	maxJob := w.MaxJob
	if maxJob == 0 {
		maxJob = 6 * time.Hour
	}
	jobCtx, cancel := context.WithTimeout(ctx, maxJob)
	defer cancel()
	hb := w.startHeartbeat(jobCtx, cancel, job)
	m, code, err := w.process(jobCtx, job, dir, hb.report)
	hb.stop()

	switch {
	case hb.stopped() || errors.Is(err, errStopped):
		w.Logger.Info("attempt stopped by the server", "attempt", job.AttemptID, "reason", hb.reason())
		return nil
	case ctx.Err() != nil:
		return ctx.Err() // shutting down; the lease will lapse and the job retries
	case errors.Is(jobCtx.Err(), context.DeadlineExceeded):
		code, err = codeTimeout, fmt.Errorf("attempt exceeded %s", maxJob)
		fallthrough
	case err != nil:
		w.Logger.Error("attempt failed", "attempt", job.AttemptID, "code", code, "err", err)
		return w.fail(ctx, job, code == codeUnsupportedSource, code, err)
	}
	// complete is safe to retry: after a commit the server answers "stop".
	if _, err := w.report(ctx, job, "complete", map[string]any{"manifest": m}); err != nil {
		return w.fail(ctx, job, false, codeCommit, err)
	}
	return nil
}

func (w *Worker) fail(ctx context.Context, job *workerapi.Job, permanent bool, code string, cause error) error {
	_, err := w.report(ctx, job, "fail", map[string]any{
		"permanent": permanent, "error_code": code, "message": truncate(cause.Error(), 500),
	})
	if err != nil {
		return fmt.Errorf("report failure %s: %w (cause: %v)", code, err, cause)
	}
	return nil
}

// report posts to an attempt endpoint, retrying transport errors and 5xx
// a few times, and treats any other non-200 as an error.
func (w *Worker) report(ctx context.Context, job *workerapi.Job, call string, body any) (int, error) {
	var status int
	var err error
	for try := 0; try < 4; try++ {
		if try > 0 {
			if serr := sleep(ctx, time.Duration(1<<(try-1))*time.Second); serr != nil {
				return 0, serr
			}
		}
		status, err = w.post(ctx, "/worker/v1/attempts/"+job.AttemptID+"/"+call, job.AttemptToken, body, nil)
		switch {
		case err == nil && status == http.StatusOK:
			return status, nil
		case err == nil && status < 500:
			return status, fmt.Errorf("%s: HTTP %d", call, status)
		case err == nil:
			err = fmt.Errorf("%s: HTTP %d", call, status)
		}
	}
	return status, err
}

// process downloads, transcodes and uploads. It returns a failure code with
// any error.
func (w *Worker) process(ctx context.Context, job *workerapi.Job, dir string, report media.Progress) (media.Manifest, string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return media.Manifest{}, codeTranscode, err
	}
	key, err := contentKey(job.Encryption)
	if err != nil {
		return media.Manifest{}, codeTranscode, err
	}
	report("download", 0)
	source := filepath.Join(dir, "source")
	if err := w.download(ctx, job, source); err != nil {
		return media.Manifest{}, codeDownload, err
	}
	tools := w.Tools.In(dir) // confined to this attempt's directory
	src, err := media.Probe(ctx, tools, source)
	if errors.Is(err, media.ErrUnsupportedSource) {
		return media.Manifest{}, codeUnsupportedSource, err
	}
	if err != nil {
		return media.Manifest{}, codeTranscode, err
	}
	out := filepath.Join(dir, "out")
	m, err := media.Transcode(ctx, tools, source, src, filepath.Join(dir, "work"), out, key, report)
	if err != nil {
		return media.Manifest{}, codeTranscode, err
	}
	report("upload", 0)
	if err := w.upload(ctx, job, out, m, report); err != nil {
		return media.Manifest{}, codeUpload, err
	}
	return m, "", nil
}

func contentKey(e workerapi.Encryption) (media.Key, error) {
	k := media.Key{Version: e.KeyVersion}
	key, err1 := hex.DecodeString(e.Key)
	kid, err2 := hex.DecodeString(e.KeyID)
	if e.Method != "aes-128" || err1 != nil || err2 != nil || len(key) != 16 || len(kid) != 16 || e.KeyVersion < 1 {
		return media.Key{}, errors.New("worker: the job's encryption block is malformed")
	}
	copy(k.Key[:], key)
	copy(k.KeyID[:], kid)
	return k, nil
}

// download fetches the source, resuming with Range after a dropped
// connection and asking sign for a fresh URL when one expires. A transfer
// that stalls for IdleTimeout counts as dropped.
func (w *Worker) download(ctx context.Context, job *workerapi.Job, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	url := job.Source.URL
	var lastErr error
	for try := 0; try < 6; try++ {
		offset, _ := f.Seek(0, io.SeekEnd)
		if offset == job.Source.Size {
			return nil
		}
		if try > 0 {
			if err := sleep(ctx, time.Duration(try)*time.Second); err != nil {
				return err
			}
		}
		reqCtx, watch := idle(ctx, w.idleTimeout())
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, w.storageURL(url), nil)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		res, err := w.httpClient().Do(req)
		if err != nil {
			watch.stop()
			lastErr = redact(err)
			continue
		}
		switch {
		case res.StatusCode == http.StatusForbidden:
			res.Body.Close()
			watch.stop()
			fresh, err := w.sign(ctx, job, nil, true)
			if err != nil {
				return err
			}
			url, lastErr = fresh.Source.URL, errors.New("source URL expired")
			continue
		case res.StatusCode == http.StatusOK && offset > 0:
			// The server ignored Range: start over.
			if err := f.Truncate(0); err != nil {
				res.Body.Close()
				watch.stop()
				return err
			}
			_, _ = f.Seek(0, io.SeekStart)
		case res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent:
			res.Body.Close()
			watch.stop()
			lastErr = fmt.Errorf("download: HTTP %d", res.StatusCode)
			continue
		}
		_, err = io.Copy(f, watch.reader(io.LimitReader(res.Body, job.Source.Size+1)))
		res.Body.Close()
		watch.stop()
		lastErr = redact(err)
		if size, _ := f.Seek(0, io.SeekEnd); size > job.Source.Size {
			return fmt.Errorf("download: got %d bytes, the source has %d", size, job.Source.Size)
		}
	}
	if offset, _ := f.Seek(0, io.SeekEnd); offset == job.Source.Size {
		return nil // the last try finished it
	}
	if lastErr == nil {
		lastErr = errors.New("source incomplete")
	}
	return fmt.Errorf("download: %w", lastErr)
}

// upload PUTs every inventoried file, then manifest.json last, through
// signed URLs, four at a time, retrying each a few times.
func (w *Worker) upload(ctx context.Context, job *workerapi.Job, outDir string, m media.Manifest, report media.Progress) error {
	paths := make([]string, 0, len(m.Files)+1)
	for _, f := range m.Files {
		paths = append(paths, f.Path)
	}
	total, done := len(paths)+1, 0
	var mu sync.Mutex
	put := func(urls map[string]string, p string) error {
		var err error
		for try := 0; try < 4; try++ {
			if try > 0 {
				if err := sleep(ctx, time.Duration(try)*time.Second); err != nil {
					return err
				}
				if errors.Is(err, errURLExpired) {
					fresh, serr := w.sign(ctx, job, []string{p}, false)
					if serr != nil {
						return serr
					}
					urls = fresh.URLs
				}
			}
			if err = w.putFile(ctx, urls[p], filepath.Join(outDir, filepath.FromSlash(p))); err == nil {
				mu.Lock()
				done++
				report("upload", float64(done)/float64(total))
				mu.Unlock()
				return nil
			}
		}
		return fmt.Errorf("upload %s: %w", p, err)
	}
	for start := 0; start < len(paths); start += workerapi.MaxSignPaths {
		batch := paths[start:min(start+workerapi.MaxSignPaths, len(paths))]
		signed, err := w.sign(ctx, job, batch, false)
		if err != nil {
			return err
		}
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		var firstErr error
		var errMu sync.Mutex
		for _, p := range batch {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				if err := put(signed.URLs, p); err != nil {
					errMu.Lock()
					firstErr = cmp(firstErr, err)
					errMu.Unlock()
				}
			})
		}
		wg.Wait()
		if firstErr != nil {
			return firstErr
		}
	}
	// The manifest goes last, so its presence means the rest landed.
	signed, err := w.sign(ctx, job, []string{"manifest.json"}, false)
	if err != nil {
		return err
	}
	return put(signed.URLs, "manifest.json")
}

func cmp(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

var errURLExpired = errors.New("signed URL expired")

func (w *Worker) putFile(ctx context.Context, url, path string) error {
	if url == "" {
		return errors.New("no signed URL")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	reqCtx, watch := idle(ctx, w.idleTimeout())
	defer watch.stop()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPut, w.storageURL(url), watch.reader(f))
	req.ContentLength = info.Size()
	// Output URLs are create-once (D016). On R2/S3 this header is signed
	// into the URL, so it must be sent; the filesystem store needs nothing.
	req.Header.Set("If-None-Match", "*")
	res, err := w.httpClient().Do(req)
	if err != nil {
		return redact(err)
	}
	res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusPreconditionFailed:
		// Outputs are create-once: an earlier try that lost its response
		// already wrote it. Commit verifies every size.
		return nil
	case http.StatusForbidden:
		return errURLExpired
	default:
		return fmt.Errorf("PUT: HTTP %d", res.StatusCode)
	}
}

func (w *Worker) idleTimeout() time.Duration {
	if w.IdleTimeout > 0 {
		return w.IdleTimeout
	}
	return time.Minute
}

func (w *Worker) httpClient() *http.Client {
	if w.HTTP != nil {
		return w.HTTP
	}
	return DefaultHTTPClient()
}

// DefaultHTTPClient bounds connecting and waiting for response headers;
// bodies are bounded per transfer by the idle watchdog instead, because
// a long upload or long-poll is legitimate.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}}
}

// redact drops the query (which carries a signature) from URL errors, so
// signed URLs never reach logs or failure reports.
func redact(err error) error {
	var ue *neturl.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	u, perr := neturl.Parse(ue.URL)
	if perr != nil {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	u.RawQuery = ""
	u.Path = redactPath(u.Path)
	return fmt.Errorf("%s %s: %w", ue.Op, u.String(), ue.Err)
}

// redactPath hides a playback token embedded in a /v/{token}/ path.
func redactPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "/v/"); ok {
		if _, after, ok := strings.Cut(rest, "/"); ok {
			return "/v/REDACTED/" + after
		}
	}
	return p
}

// sign asks the server for PUT URLs or a fresh source URL. A stop verdict
// becomes errStopped.
func (w *Worker) sign(ctx context.Context, job *workerapi.Job, paths []string, source bool) (workerapi.SignResponse, error) {
	var out workerapi.SignResponse
	status, err := w.post(ctx, "/worker/v1/attempts/"+job.AttemptID+"/sign", job.AttemptToken,
		map[string]any{"paths": paths, "source": source}, &out)
	switch {
	case err != nil:
		return out, err
	case status != http.StatusOK:
		return out, fmt.Errorf("sign: HTTP %d", status)
	case out.Action == "stop":
		return out, errStopped
	}
	return out, nil
}

// post sends a JSON request and decodes a JSON response into out.
func (w *Worker) post(ctx context.Context, path, bearer string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(w.Server, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := w.httpClient().Do(req)
	if err != nil {
		return 0, redact(err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); err != nil {
			return res.StatusCode, fmt.Errorf("%s: %w", path, err)
		}
	}
	return res.StatusCode, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
