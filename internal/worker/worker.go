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
	"net/http"
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
}

var errStopped = errors.New("worker: the server told this attempt to stop")

// Failure codes the worker reports.
const (
	codeUnsupportedSource = "unsupported_source"
	codeDownload          = "source_download_failed"
	codeTranscode         = "transcode_failed"
	codeUpload            = "upload_failed"
	codeCommit            = "commit_failed"
)

// Run claims and runs jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.Logger.Error("worker", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
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
	}, &job)
	switch {
	case err != nil:
		return nil, err
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

	jobCtx, cancel := context.WithCancel(ctx)
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
	case err != nil:
		w.Logger.Error("attempt failed", "attempt", job.AttemptID, "code", code, "err", err)
		return w.fail(ctx, job, code == codeUnsupportedSource, code, err)
	}
	status, err := w.post(ctx, "/worker/v1/attempts/"+job.AttemptID+"/complete", job.AttemptToken, map[string]any{"manifest": m}, nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("complete: HTTP %d", status)
	}
	if err != nil {
		return w.fail(ctx, job, false, codeCommit, err)
	}
	return nil
}

func (w *Worker) fail(ctx context.Context, job *workerapi.Job, permanent bool, code string, cause error) error {
	_, err := w.post(ctx, "/worker/v1/attempts/"+job.AttemptID+"/fail", job.AttemptToken, map[string]any{
		"permanent": permanent, "error_code": code, "message": truncate(cause.Error(), 500),
	}, nil)
	if err != nil {
		return fmt.Errorf("report failure %s: %w (cause: %v)", code, err, cause)
	}
	return nil
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
	src, err := media.Probe(ctx, w.Tools, source)
	if errors.Is(err, media.ErrUnsupportedSource) {
		return media.Manifest{}, codeUnsupportedSource, err
	}
	if err != nil {
		return media.Manifest{}, codeTranscode, err
	}
	out := filepath.Join(dir, "out")
	m, err := media.Transcode(ctx, w.Tools, source, src, filepath.Join(dir, "work"), out, key, report)
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
// connection and asking sign for a fresh URL when one expires.
func (w *Worker) download(ctx context.Context, job *workerapi.Job, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	url := job.Source.URL
	var lastErr error
	for try := 0; try < 6; try++ {
		if try > 0 {
			if err := sleep(ctx, time.Duration(try)*time.Second); err != nil {
				return err
			}
		}
		offset, _ := f.Seek(0, io.SeekEnd)
		if offset == job.Source.Size {
			return nil
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		res, err := w.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case res.StatusCode == http.StatusForbidden:
			res.Body.Close()
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
				return err
			}
			_, _ = f.Seek(0, io.SeekStart)
		case res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent:
			res.Body.Close()
			lastErr = fmt.Errorf("download: HTTP %d", res.StatusCode)
			continue
		}
		_, err = io.Copy(f, io.LimitReader(res.Body, job.Source.Size+1))
		res.Body.Close()
		lastErr = err
		if size, _ := f.Seek(0, io.SeekEnd); size > job.Source.Size {
			return fmt.Errorf("download: got %d bytes, the source has %d", size, job.Source.Size)
		}
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	req.ContentLength = info.Size()
	res, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusForbidden:
		return errURLExpired
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("PUT: HTTP %d", res.StatusCode)
	}
	return nil
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
	res, err := w.HTTP.Do(req)
	if err != nil {
		return 0, err
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
