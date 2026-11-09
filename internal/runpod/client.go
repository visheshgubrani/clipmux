// Package runpod is the RunPod executor (P4-T04, D056): on-demand pods
// started for one dispatch each through RunPod's REST API
// (https://rest.runpod.io/v1), and the Connect RunPod steps.
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
	"strings"
	"time"
)

// DefaultAPI is RunPod's REST API. CLIPMUX_RUNPOD_API replaces it, for the
// fake in tests.
const DefaultAPI = "https://rest.runpod.io/v1"

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

// reason is RunPod's error text from a refused call's body, masked.
func reason(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Error == "" {
		return ""
	}
	msg := tokenLike.ReplaceAllString(body.Error, "…")
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

// PodInput is the part of POST /pods ClipMux sets.
type PodInput struct {
	Name                    string            `json:"name"`
	ImageName               string            `json:"imageName"`
	ComputeType             string            `json:"computeType"`
	CloudType               string            `json:"cloudType"`
	GPUTypeIDs              []string          `json:"gpuTypeIds,omitempty"`
	GPUTypePriority         string            `json:"gpuTypePriority,omitempty"`
	GPUCount                int               `json:"gpuCount,omitempty"`
	Env                     map[string]string `json:"env"`
	DockerStartCmd          []string          `json:"dockerStartCmd"`
	ContainerDiskInGb       int               `json:"containerDiskInGb"`
	VolumeInGb              int               `json:"volumeInGb"`
	Ports                   []string          `json:"ports"`
	ContainerRegistryAuthID string            `json:"containerRegistryAuthId,omitempty"`
	Interruptible           bool              `json:"interruptible"`
}

// Pod is the part of RunPod's pod object ClipMux reads.
type Pod struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DesiredStatus string `json:"desiredStatus"`
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

// ListPods lists the account's pods, named name when it isn't empty.
func (c *Client) ListPods(ctx context.Context, name string) ([]Pod, error) {
	path := "/pods"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}
	var pods []Pod
	err := c.do(ctx, "list pods", http.MethodGet, path, nil, &pods)
	return pods, err
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
	err := c.do(ctx, "create registry auth", http.MethodPost, "/containerregistryauth",
		map[string]string{"name": name, "username": username, "password": password}, &out)
	if err == nil && out.ID == "" {
		err = errors.New("runpod create registry auth: no ID in the response")
	}
	return out.ID, err
}

// DeleteRegistryAuth removes stored registry credentials. Gone is fine.
func (c *Client) DeleteRegistryAuth(ctx context.Context, id string) error {
	err := c.do(ctx, "delete registry auth", http.MethodDelete, "/containerregistryauth/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
