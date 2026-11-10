// Package cloudflare is Connect Cloudflare (docs/architecture.md, "Optional
// upgrades"): one API token and a media hostname become an R2 bucket, the
// ClipMux Worker deployed on that hostname, and Worker delivery for the
// project. The raw token is used during the request and never stored.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// DefaultAPI is Cloudflare's v4 API.
const DefaultAPI = "https://api.cloudflare.com/client/v4"

// Client calls the Cloudflare API with one token.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// APIError is a failed Cloudflare call. It names the call and Cloudflare's
// codes, never the token.
type APIError struct {
	Op     string
	Status int
	Codes  []int
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudflare %s: HTTP %d %v %s", e.Op, e.Status, e.Codes, e.Msg)
}

// NotFound reports a 404 from Cloudflare.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiMessage    `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, op, method, path string, body io.Reader, contentType string, out any) error {
	base := c.Base
	if base == "" {
		base = DefaultAPI
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare %s: %w", op, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("cloudflare %s: %w", op, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || res.StatusCode >= 300 || !env.Success {
		e := &APIError{Op: op, Status: res.StatusCode}
		var msgs []string
		for _, m := range env.Errors {
			e.Codes = append(e.Codes, m.Code)
			msgs = append(msgs, m.Message)
		}
		e.Msg = strings.Join(msgs, "; ")
		return e
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("cloudflare %s: %w", op, err)
		}
	}
	return nil
}

func (c *Client) json(ctx context.Context, op, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, op, method, path, body, ct, out)
}

// VerifyToken returns the token's ID if it is active.
func (c *Client) VerifyToken(ctx context.Context) (string, error) {
	return c.verifyToken(ctx, "/user/tokens/verify")
}

// VerifyAccountToken verifies an account-owned token at its account endpoint.
func (c *Client) VerifyAccountToken(ctx context.Context, accountID string) (string, error) {
	return c.verifyToken(ctx, "/accounts/"+url.PathEscape(accountID)+"/tokens/verify")
}

func (c *Client) verifyToken(ctx context.Context, path string) (string, error) {
	var r struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := c.json(ctx, "verify token", http.MethodGet, path, nil, &r); err != nil {
		return "", err
	}
	if r.Status != "active" || r.ID == "" {
		return "", fmt.Errorf("cloudflare token is %q, not active", r.Status)
	}
	return r.ID, nil
}

// Account is a Cloudflare account the token can reach.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Accounts lists the accounts the token reaches.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var a []Account
	return a, c.json(ctx, "list accounts", http.MethodGet, "/accounts?per_page=50", nil, &a)
}

// Zone is a DNS zone.
type Zone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

// ZoneFor finds the zone holding hostname: its longest suffix that is a
// zone of the account.
func (c *Client) ZoneFor(ctx context.Context, accountID, hostname string) (Zone, error) {
	labels := strings.Split(hostname, ".")
	for i := 0; i < len(labels)-1; i++ {
		name := strings.Join(labels[i:], ".")
		var zs []Zone
		q := url.Values{"name": {name}, "account.id": {accountID}}
		if err := c.json(ctx, "find zone", http.MethodGet, "/zones?"+q.Encode(), nil, &zs); err != nil {
			return Zone{}, err
		}
		for _, z := range zs {
			if z.Name == name {
				return z, nil
			}
		}
	}
	return Zone{}, fmt.Errorf("no zone for %s in this account; add the domain to Cloudflare first", hostname)
}

// WorkersPaid reports whether the account has a Workers Paid subscription.
func (c *Client) WorkersPaid(ctx context.Context, accountID string) (bool, error) {
	var subs []struct {
		RatePlan struct {
			ID         string `json:"id"`
			PublicName string `json:"public_name"`
		} `json:"rate_plan"`
		State string `json:"state"`
	}
	if err := c.json(ctx, "list subscriptions", http.MethodGet, "/accounts/"+accountID+"/subscriptions", nil, &subs); err != nil {
		return false, err
	}
	for _, s := range subs {
		name := strings.ToLower(s.RatePlan.ID + " " + s.RatePlan.PublicName)
		if strings.Contains(name, "workers") && strings.Contains(name, "paid") && !strings.EqualFold(s.State, "cancelled") {
			return true, nil
		}
	}
	return false, nil
}

// EnsureBucket creates the R2 bucket unless it exists. created says which.
func (c *Client) EnsureBucket(ctx context.Context, accountID, name string) (created bool, err error) {
	err = c.json(ctx, "get bucket", http.MethodGet, "/accounts/"+accountID+"/r2/buckets/"+url.PathEscape(name), nil, nil)
	var apiErr *APIError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &apiErr) && apiErr.NotFound():
		return true, c.json(ctx, "create bucket", http.MethodPost, "/accounts/"+accountID+"/r2/buckets", map[string]string{"name": name}, nil)
	}
	return false, err
}

// Binding is one Worker binding.
type Binding struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	BucketName string `json:"bucket_name,omitempty"`
	Text       string `json:"text,omitempty"`
	Dataset    string `json:"dataset,omitempty"`
}

// UploadWorker deploys a module Worker with its bindings. Secret bindings
// travel in the upload and Cloudflare keeps them encrypted.
func (c *Client) UploadWorker(ctx context.Context, accountID, script string, module []byte, compatibilityDate string, bindings []Binding) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	meta, err := json.Marshal(map[string]any{
		"main_module":        "worker.js",
		"compatibility_date": compatibilityDate,
		"bindings":           bindings,
	})
	if err != nil {
		return err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(h)
	if err == nil {
		_, err = part.Write(meta)
	}
	if err == nil {
		h = textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="worker.js"; filename="worker.js"`)
		h.Set("Content-Type", "application/javascript+module")
		if part, err = mw.CreatePart(h); err == nil {
			_, err = part.Write(module)
		}
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return err
	}
	return c.do(ctx, "upload worker", http.MethodPut, "/accounts/"+accountID+"/workers/scripts/"+url.PathEscape(script), &body, mw.FormDataContentType(), nil)
}

// DNSRecord is a record in a zone.
type DNSRecord struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

// DomainMatches reports whether hostname is already attached to this Worker.
// Cloudflare owns its DNS records then; they cannot be deleted through DNS API.
func (c *Client) DomainMatches(ctx context.Context, accountID, zoneID, hostname, script string) (bool, error) {
	var domains []struct {
		Hostname string `json:"hostname"`
		Service  string `json:"service"`
		ZoneID   string `json:"zone_id"`
	}
	query := url.Values{"hostname": {hostname}, "zone_id": {zoneID}, "service": {script}}
	if err := c.json(ctx, "list worker domains", http.MethodGet, "/accounts/"+accountID+"/workers/domains?"+query.Encode(), nil, &domains); err != nil {
		return false, err
	}
	for _, domain := range domains {
		if domain.Hostname == hostname && domain.Service == script && domain.ZoneID == zoneID {
			return true, nil
		}
	}
	return false, nil
}

// ClearHostname deletes the DNS records at hostname (the ones that pointed
// the media hostname at the app), because a Worker custom domain can't be
// attached over an existing record.
func (c *Client) ClearHostname(ctx context.Context, zoneID, hostname string) (int, error) {
	var recs []DNSRecord
	q := url.Values{"name": {hostname}}
	if err := c.json(ctx, "list dns records", http.MethodGet, "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &recs); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if r.Name != hostname {
			continue
		}
		if err := c.json(ctx, "delete dns record", http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+r.ID, nil, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// AttachDomain serves the Worker on hostname as a custom domain.
func (c *Client) AttachDomain(ctx context.Context, accountID, zoneID, hostname, script string) error {
	return c.json(ctx, "attach domain", http.MethodPut, "/accounts/"+accountID+"/workers/domains", map[string]string{
		"hostname": hostname, "service": script, "zone_id": zoneID, "environment": "production",
	}, nil)
}
