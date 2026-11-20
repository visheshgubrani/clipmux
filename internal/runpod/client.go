// Package runpod is the RunPod executor (P4-T04, D056): on-demand pods
// started for one dispatch each through RunPod's REST API v2
// (https://api.runpod.io/v2, D139), and the Connect RunPod steps.
package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// DefaultAPI is RunPod's REST API v2. CLIPMUX_RUNPOD_API replaces it, for
// the fake in tests. v1 (rest.runpod.io/v1) was retired on 2026-11-15.
const DefaultAPI = "https://api.runpod.io/v2"

// Client calls RunPod's REST API with one API key.
type Client struct {
	Base string
	Key  string
	HTTP *http.Client
}

// NewClient returns a client with a bounded timeout per call.
func NewClient(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// APIError is a refused call: its status and RunPod's reason. The reason
// may echo what was sent, which can include a key or a registry password,
// so every run of 20 or more token characters in it is masked (each secret
// ClipMux sends is longer), and it is cut at 300 bytes.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("runpod %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("runpod %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

var tokenLike = regexp.MustCompile(`[A-Za-z0-9_-]{20,}`)

// reason is RunPod's error text from a refused call's body, masked: an
// RFC 9457 problem's detail, then each validation error under it.
func reason(raw []byte) string {
	var body struct {
		Detail string   `json:"detail"`
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	parts := slices.DeleteFunc(append([]string{body.Detail}, body.Errors...), func(s string) bool { return s == "" })
	msg := tokenLike.ReplaceAllString(strings.Join(parts, "; "), "…")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// ErrNotFound is a 404: a pod or registry auth that is already gone.
var ErrNotFound = errors.New("runpod: not found")

func (c *Client) do(ctx context.Context, op, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		// The transport error names the URL, never the key.
		return fmt.Errorf("runpod %s: %w", op, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("runpod %s: %w", op, err)
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, &APIError{Op: op, Status: res.StatusCode, Message: reason(raw)})
	case res.StatusCode < 200 || res.StatusCode > 299:
		return &APIError{Op: op, Status: res.StatusCode, Message: reason(raw)}
	case out == nil || len(raw) == 0:
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("runpod %s: unreadable response: %w", op, err)
	}
	return nil
}

// PodInput is the part of RunPod's CreatePodRequest ClipMux sets. v2
// refuses a field it doesn't know, and with no mounts a pod has no volume.
type PodInput struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	// Cloud is SECURE or COMMUNITY.
	Cloud string `json:"cloud,omitempty"`
	// GPU is one type: v2 places a pod on exactly the type asked for.
	GPU *PodGPU           `json:"gpu,omitempty"`
	Env map[string]string `json:"env,omitempty"`
	// Cmd is the arguments to the image's entrypoint, in exec form.
	Cmd []string `json:"cmd,omitempty"`
	// Disk is the container disk in GB.
	Disk int `json:"disk,omitempty"`
	// Ports are the ports exposed; v2 has no default.
	Ports []string `json:"ports"`
	// Registry is the ID of RunPod's stored credentials for a private image.
	Registry string `json:"registry,omitempty"`
}

// PodGPU is a pod's GPU request.
type PodGPU struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// Pod is the part of RunPod's pod object ClipMux reads. Status is the pod's
// actual state: PROVISIONING, STARTING, RUNNING, EXITED, ERROR or
// TERMINATED.
type Pod struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// CreatePod starts a pod.
func (c *Client) CreatePod(ctx context.Context, in PodInput) (Pod, error) {
	var p Pod
	err := c.do(ctx, "create pod", http.MethodPost, "/pods", in, &p)
	if err == nil && p.ID == "" {
		err = errors.New("runpod create pod: no pod ID in the response")
	}
	return p, err
}

// maxPages bounds a pod list, at RunPod's 1000 pods a page, should a
// cursor never end.
const maxPages = 100

// ListPods lists every pod in the account, a page at a time. v2 ignores
// filters it doesn't know (such as name), so callers match names
// themselves.
func (c *Client) ListPods(ctx context.Context) ([]Pod, error) {
	var pods []Pod
	cursor := ""
	for range maxPages {
		path := "/pods"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Pods       []Pod `json:"pods"`
			Pagination struct {
				NextCursor  string `json:"nextCursor"`
				HasNextPage bool   `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := c.do(ctx, "list pods", http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		pods = append(pods, page.Pods...)
		if !page.Pagination.HasNextPage || page.Pagination.NextCursor == "" {
			return pods, nil
		}
		cursor = page.Pagination.NextCursor
	}
	return nil, fmt.Errorf("runpod list pods: more than %d pages", maxPages)
}

// DeletePod terminates a pod. One that is already gone is fine.
func (c *Client) DeletePod(ctx context.Context, id string) error {
	err := c.do(ctx, "delete pod", http.MethodDelete, "/pods/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// CreateRegistryAuth stores registry credentials in RunPod, so pods can
// pull a private image; it returns their ID. Names must be unique.
func (c *Client) CreateRegistryAuth(ctx context.Context, name, username, password string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, "create registry auth", http.MethodPost, "/registries",
		map[string]string{"name": name, "username": username, "password": password}, &out)
	if err == nil && out.ID == "" {
		err = errors.New("runpod create registry auth: no ID in the response")
	}
	return out.ID, err
}

// DeleteRegistryAuth removes stored registry credentials. Gone is fine.
func (c *Client) DeleteRegistryAuth(ctx context.Context, id string) error {
	err := c.do(ctx, "delete registry auth", http.MethodDelete, "/registries/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
