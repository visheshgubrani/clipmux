package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AgentConfig is what a paired agent keeps on disk: the server it belongs
// to and its key. The file is 0600 and the key never appears in argv.
type AgentConfig struct {
	Server  string `json:"server"`
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	// Roots, when set, are the only folders push --local may hand over.
	Roots []string `json:"roots,omitempty"`
}

// ConfigPath is where the agent's config lives in dir.
func ConfigPath(dir string) string { return filepath.Join(dir, "agent.json") }

// LoadAgentConfig reads a saved config; a missing file is os.ErrNotExist.
func LoadAgentConfig(dir string) (AgentConfig, error) {
	raw, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		return AgentConfig{}, err
	}
	var c AgentConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return AgentConfig{}, fmt.Errorf("read %s: %w", ConfigPath(dir), err)
	}
	if c.Server == "" || c.Key == "" {
		return AgentConfig{}, fmt.Errorf("read %s: no server or key", ConfigPath(dir))
	}
	return c, nil
}

// SaveAgentConfig writes the config atomically, readable only by its owner.
func SaveAgentConfig(dir string, c AgentConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("save agent config: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agent-*.json")
	if err != nil {
		return fmt.Errorf("save agent config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("save agent config: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("save agent config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save agent config: %w", err)
	}
	if err := os.Rename(tmp.Name(), ConfigPath(dir)); err != nil {
		return fmt.Errorf("save agent config: %w", err)
	}
	return nil
}

// Describe is how this machine introduces itself when pairing.
func Describe(name string) map[string]string {
	host, _ := os.Hostname()
	return map[string]string{"name": name, "hostname": host, "platform": runtime.GOOS + "/" + runtime.GOARCH}
}

// PairingError is a refusal from the server, such as an expired code.
type PairingError struct {
	Status        int
	Code, Message string
}

func (e *PairingError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("pairing refused: HTTP %d", e.Status)
}

// call posts JSON to an unauthenticated pairing route.
func call(ctx context.Context, client *http.Client, server, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(server, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return redact(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &e) // a non-JSON error still reports its status
		return &PairingError{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

type credentials struct {
	AgentID string `json:"agent_id"`
	Key     string `json:"key"`
	Name    string `json:"name"`
}

func (c credentials) config(server string) AgentConfig {
	return AgentConfig{Server: strings.TrimSuffix(server, "/"), AgentID: c.AgentID, Name: c.Name, Key: c.Key}
}

// Redeem exchanges a code minted on the Workers page for this agent's key.
func Redeem(ctx context.Context, client *http.Client, server, code, name string) (AgentConfig, error) {
	body := map[string]string{"code": code}
	for k, v := range Describe(name) {
		body[k] = v
	}
	var c credentials
	if err := call(ctx, client, server, "/worker/v1/pair", body, &c); err != nil {
		return AgentConfig{}, err
	}
	return c.config(server), nil
}

// PairingRequest is an agent-started pairing waiting for the owner.
type PairingRequest struct {
	Code      string    `json:"code"`
	PollToken string    `json:"poll_token"`
	VerifyURL string    `json:"verify_url"`
	ExpiresAt time.Time `json:"expires_at"`
	Interval  int       `json:"interval_seconds"`
}

// ErrPairingDenied and ErrPairingExpired end an agent-started pairing.
var (
	ErrPairingDenied  = errors.New("the owner denied this machine")
	ErrPairingExpired = errors.New("the pairing code expired before it was approved")
)

// StartPairing asks the server for a code to show the owner.
func StartPairing(ctx context.Context, client *http.Client, server, name string) (PairingRequest, error) {
	var p PairingRequest
	err := call(ctx, client, server, "/worker/v1/pairings", Describe(name), &p)
	return p, err
}

// AwaitApproval polls until the owner approves or denies the pairing, or
// it expires. Network errors are retried: a laptop may drop off Wi-Fi while
// the owner finds the dashboard.
func AwaitApproval(ctx context.Context, client *http.Client, server string, p PairingRequest) (AgentConfig, error) {
	every := time.Duration(max(p.Interval, 1)) * time.Second
	for {
		var res struct {
			Status      string       `json:"status"`
			Credentials *credentials `json:"credentials"`
		}
		err := call(ctx, client, server, "/worker/v1/pairings/poll", map[string]string{"poll_token": p.PollToken}, &res)
		var pe *PairingError
		switch {
		case errors.As(err, &pe):
			return AgentConfig{}, err
		case err != nil:
			// retried below
		case res.Status == "approved" && res.Credentials != nil:
			return res.Credentials.config(server), nil
		case res.Status == "denied":
			return AgentConfig{}, ErrPairingDenied
		case res.Status == "expired":
			return AgentConfig{}, ErrPairingExpired
		}
		select {
		case <-ctx.Done():
			return AgentConfig{}, ctx.Err()
		case <-time.After(every):
		}
	}
}
