// Package push uploads files from any machine through ClipMux's public
// upload API, the same calls a browser makes (docs/api.md, "Integration
// API"): create the upload with an API key, send 32 MiB parts straight to
// storage, complete. It follows @clipmux/uploader's reliability rules (D031):
// storage is the source of truth for which parts landed, so a push that was
// killed resumes by listing parts and sending the rest, and a re-run of a
// finished push creates nothing new.
package push

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/visheshgubrani/clipmux/internal/logging"
)

const (
	// signBatch is the most part URLs one call may sign.
	signBatch = 100
	// urlMargin re-signs a part URL this close to its expiry.
	urlMargin = 5 * time.Minute
	// maxTries bounds each request; waits are min(30 s, 1 s·2ⁿ) plus jitter.
	maxTries = 6
)

// Client talks to one ClipMux server as one app.
type Client struct {
	Server string // the public URL
	Key    string // an API key with uploads:write (and assets:read to wait)
	HTTP   *http.Client
	// StateDir keeps one small file per unfinished push, for resuming.
	StateDir string
	// Concurrency is the parts in flight at once (default 4).
	Concurrency int
	// Sleep waits between retries; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Options describe the asset a push creates.
type Options struct {
	Policy      string // signed (default) or public
	Passthrough string
	// Progress, when set, is told the bytes stored so far.
	Progress func(done, total int64)
}

// PlaybackID is one of an asset's playback IDs.
type PlaybackID struct {
	ID       string `json:"id"`
	Policy   string `json:"policy"`
	Playable bool   `json:"playable"`
}

// Asset is the asset view the API returns.
type Asset struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	ErrorCode   string       `json:"error_code"`
	BaseURL     string       `json:"base_url"`
	PlaybackIDs []PlaybackID `json:"playback_ids"`
}

// Error is a refusal from the API, with its stable code.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message) }

type upload struct {
	UploadID    string `json:"upload_id"`
	AssetID     string `json:"asset_id"`
	UploadToken string `json:"upload_token"`
	PartSize    int64  `json:"part_size"`
	PartCount   int    `json:"part_count"`
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wait is the retry delay before try n (1-based), honouring Retry-After.
func wait(n int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(retryAfter); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	d := min(30*time.Second, time.Second<<(n-1))
	return d + time.Duration(rand.Int64N(int64(time.Second)))
}

func retryable(status int) bool {
	return status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// call sends a JSON API request with retries and decodes a 2xx body into
// out. bearer is the API key or an upload token.
func (c *Client) call(ctx context.Context, method, path, bearer string, header map[string]string, body, out any) (int, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	for try := 1; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Server, "/")+path, bytes.NewReader(raw))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, err := c.client().Do(req)
		if err != nil {
			if ctx.Err() != nil || try >= maxTries {
				return 0, fmt.Errorf("%s %s: %w", method, path, err)
			}
			if err := c.sleep(ctx, wait(try, "")); err != nil {
				return 0, err
			}
			continue
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if err != nil {
			return res.StatusCode, err
		}
		if retryable(res.StatusCode) && try < maxTries {
			if err := c.sleep(ctx, wait(try, res.Header.Get("Retry-After"))); err != nil {
				return 0, err
			}
			continue
		}
		if res.StatusCode/100 != 2 {
			var e struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			_ = json.Unmarshal(data, &e) // a non-JSON error still reports its status
			return res.StatusCode, &Error{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return res.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
			}
		}
		return res.StatusCode, nil
	}
}

// identity names one push of one file: the same file pushed again with the
// same options to the same server is the same push, so it resumes or
// replays instead of creating a second asset.
func (c *Client) identity(abs string, fi os.FileInfo, o Options) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"clipmux-push-v1", strings.TrimSuffix(c.Server, "/"), abs, strconv.FormatInt(fi.Size(), 10),
		strconv.FormatInt(fi.ModTime().UnixNano(), 10), o.Policy, o.Passthrough,
	}, "\n")))
	return hex.EncodeToString(sum[:16])
}

func (c *Client) statePath(id string) string { return filepath.Join(c.StateDir, id+".json") }

func (c *Client) loadState(id string) (upload, bool) {
	raw, err := os.ReadFile(c.statePath(id))
	if err != nil {
		return upload{}, false
	}
	var u upload
	if json.Unmarshal(raw, &u) != nil || u.UploadID == "" || u.UploadToken == "" || u.PartSize <= 0 {
		return upload{}, false
	}
	return u, true
}

// saveState keeps the upload token beside the user's files (0600) so a
// killed push can resume; it is deleted once the push completes.
func (c *Client) saveState(id string, u upload) error {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return fmt.Errorf("save push state: %w", err)
	}
	raw, _ := json.Marshal(u) // a struct of strings and ints always marshals
	tmp := c.statePath(id) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("save push state: %w", err)
	}
	return os.Rename(tmp, c.statePath(id))
}

// File pushes one file and returns its asset, processing or later.
func (c *Client) File(ctx context.Context, path string, o Options) (Asset, error) {
	return c.file(ctx, path, o, "")
}

// file pushes with an Idempotency-Key suffix: empty at first, a fresh one
// when the earlier upload expired or was cancelled and must start over.
func (c *Client) file(ctx context.Context, path string, o Options, restart string) (Asset, error) {
	if o.Policy == "" {
		o.Policy = "signed"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Asset{}, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return Asset{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Asset{}, err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return Asset{}, fmt.Errorf("%s: not a non-empty regular file", path)
	}
	id := c.identity(abs, fi, o)

	u, ok := c.loadState(id)
	if !ok {
		// A replay of an earlier create (same Idempotency-Key) returns the
		// same upload with a fresh token, or no token once it finished.
		var created upload
		_, err := c.call(ctx, http.MethodPost, "/v1/uploads", c.Key, map[string]string{"Idempotency-Key": "push-" + id + restart},
			map[string]any{"size": fi.Size(), "playback_policy": o.Policy, "passthrough": o.Passthrough}, &created)
		if err != nil {
			return Asset{}, err
		}
		if created.UploadToken == "" {
			return c.Asset(ctx, created.AssetID)
		}
		u = created
		if err := c.saveState(id, u); err != nil {
			return Asset{}, err
		}
	}
	stored, finished, err := c.listParts(ctx, u)
	var e *Error
	switch {
	case restart == "" && errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusNotFound):
		// The upload expired or was cancelled: forget it and start over.
		_ = os.Remove(c.statePath(id))
		return c.file(ctx, path, o, "-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	case err != nil:
		return Asset{}, err
	case finished:
		// Completed (by the sweeper, say) or ended: completing again returns
		// the asset of a completed upload; an expired or cancelled one
		// starts over.
		a, err := c.complete(ctx, id, u, nil)
		if errors.As(err, &e) && e.Code == "upload_not_active" && restart == "" {
			_ = os.Remove(c.statePath(id))
			return c.file(ctx, path, o, "-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		}
		return a, err
	}

	plan := planParts(fi.Size(), u.PartSize)
	var missing []part
	var done int64
	for _, p := range plan {
		if s, ok := stored[p.Number]; ok && s.Size == p.Size {
			done += p.Size
			continue
		}
		missing = append(missing, p)
	}
	if o.Progress != nil {
		o.Progress(done, fi.Size())
	}
	etags, err := c.send(ctx, f, u, missing, done, fi.Size(), o.Progress)
	if err != nil {
		return Asset{}, err
	}
	for n, s := range stored {
		if _, ok := etags[n]; !ok {
			etags[n] = s.ETag
		}
	}
	return c.complete(ctx, id, u, etags)
}

type part struct {
	Number int    `json:"part_number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
	Start  int64  `json:"-"`
}

func planParts(size, partSize int64) []part {
	var out []part
	for start, n := int64(0), 1; start < size; start, n = start+partSize, n+1 {
		out = append(out, part{Number: n, Start: start, Size: min(partSize, size-start)})
	}
	return out
}

// listParts asks what storage holds. A 409 means the upload already
// completed (the sweeper finishes uploads whose parts all landed).
func (c *Client) listParts(ctx context.Context, u upload) (map[int]part, bool, error) {
	var body struct {
		Parts []part `json:"parts"`
	}
	_, err := c.call(ctx, http.MethodGet, "/v1/uploads/"+u.UploadID+"/parts", u.UploadToken, nil, nil, &body)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusConflict {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := make(map[int]part, len(body.Parts))
	for _, p := range body.Parts {
		out[p.Number] = p
	}
	return out, false, nil
}

func (c *Client) complete(ctx context.Context, id string, u upload, etags map[int]string) (Asset, error) {
	parts := make([]map[string]any, 0, len(etags))
	for _, n := range slices.Sorted(maps.Keys(etags)) {
		parts = append(parts, map[string]any{"part_number": n, "etag": etags[n]})
	}
	var a Asset
	if _, err := c.call(ctx, http.MethodPost, "/v1/uploads/"+u.UploadID+"/complete", u.UploadToken, nil,
		map[string]any{"parts": parts}, &a); err != nil {
		return Asset{}, err
	}
	_ = os.Remove(c.statePath(id)) // finished; a leftover file only costs a list-parts call
	return a, nil
}

type signed struct {
	url     string
	expires time.Time
}

// send PUTs the missing parts, Concurrency at a time, signing URLs in
// batches just before they're needed.
func (c *Client) send(ctx context.Context, f *os.File, u upload, missing []part, done, total int64, progress func(int64, int64)) (map[int]string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		mu    sync.Mutex
		urls  = map[int]signed{}
		etags = map[int]string{}
		next  int
	)
	sign := func(from int, force int) error {
		var nums []int
		for i := from; i < len(missing) && len(nums) < signBatch; i++ {
			n := missing[i].Number
			if s, ok := urls[n]; n == force || !ok || time.Until(s.expires) < urlMargin {
				nums = append(nums, n)
			}
		}
		if len(nums) == 0 {
			return nil
		}
		var body struct {
			Parts []struct {
				Number int    `json:"part_number"`
				URL    string `json:"url"`
			} `json:"parts"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if _, err := c.call(ctx, http.MethodPost, "/v1/uploads/"+u.UploadID+"/parts", u.UploadToken, nil,
			map[string]any{"part_numbers": nums}, &body); err != nil {
			return err
		}
		for _, p := range body.Parts {
			urls[p.Number] = signed{url: p.URL, expires: body.ExpiresAt}
		}
		return nil
	}
	urlFor := func(i int, force bool) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n := missing[i].Number
		s, ok := urls[n]
		if force || !ok || time.Until(s.expires) < urlMargin {
			f := 0
			if force {
				f = n
			}
			if err := sign(i, f); err != nil {
				return "", err
			}
			s = urls[n]
		}
		return s.url, nil
	}

	workers := c.Concurrency
	if workers <= 0 {
		workers = 4
	}
	var wg sync.WaitGroup
	for range min(workers, len(missing)) {
		wg.Go(func() {
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= len(missing) || ctx.Err() != nil {
					return
				}
				p := missing[i]
				etag, err := c.put(ctx, f, p, func(force bool) (string, error) { return urlFor(i, force) })
				if err != nil {
					cancel(fmt.Errorf("part %d: %w", p.Number, err))
					return
				}
				mu.Lock()
				etags[p.Number] = etag
				done += p.Size
				if progress != nil {
					progress(done, total)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return etags, nil
}

// put sends one part, retrying network errors, 5xx, 408 and 429, and
// signing again once after a 403 (an expired URL).
func (c *Client) put(ctx context.Context, f *os.File, p part, urlFor func(force bool) (string, error)) (string, error) {
	resigned := false
	for try := 1; ; {
		url, err := urlFor(resigned && try == 1)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, io.NewSectionReader(f, p.Start, p.Size))
		if err != nil {
			return "", err
		}
		req.ContentLength = p.Size
		res, err := c.client().Do(req)
		if err != nil {
			if ctx.Err() != nil || try >= maxTries {
				return "", logging.RedactError(err)
			}
			if err := c.sleep(ctx, wait(try, "")); err != nil {
				return "", err
			}
			try++
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		switch {
		case res.StatusCode/100 == 2:
			etag := res.Header.Get("ETag")
			if etag == "" {
				return "", errors.New("storage answered without an ETag header; its CORS rules must expose ETag")
			}
			return etag, nil
		case res.StatusCode == http.StatusForbidden && !resigned:
			resigned, try = true, 1
			continue
		case retryable(res.StatusCode) && try < maxTries:
			if err := c.sleep(ctx, wait(try, res.Header.Get("Retry-After"))); err != nil {
				return "", err
			}
			try++
			continue
		}
		return "", fmt.Errorf("storage refused the part: HTTP %d", res.StatusCode)
	}
}

// AttachLocal creates the asset for a file a PC agent registered for push
// --local (POST /v1/assets with agent_source_id), as the key's app.
func (c *Client) AttachLocal(ctx context.Context, sourceID string, o Options) (Asset, error) {
	if o.Policy == "" {
		o.Policy = "signed"
	}
	var a Asset
	_, err := c.call(ctx, http.MethodPost, "/v1/assets", c.Key, map[string]string{"Idempotency-Key": "push-local-" + sourceID},
		map[string]any{"agent_source_id": sourceID, "playback_policy": o.Policy, "passthrough": o.Passthrough}, &a)
	return a, err
}

// Asset reads an asset.
func (c *Client) Asset(ctx context.Context, id string) (Asset, error) {
	var a Asset
	_, err := c.call(ctx, http.MethodGet, "/v1/assets/"+id, c.Key, nil, nil, &a)
	return a, err
}

// Wait polls an asset until it is ready or errored.
func (c *Client) Wait(ctx context.Context, id string, every time.Duration) (Asset, error) {
	for {
		a, err := c.Asset(ctx, id)
		if err != nil || a.Status == "ready" || a.Status == "errored" || a.Status == "deleted" {
			return a, err
		}
		if err := c.sleep(ctx, every); err != nil {
			return a, err
		}
	}
}
