// Package registry resolves a container image reference to its digest
// through the OCI distribution API (D058), so a provider's machines run
// exactly the image the owner connected, and Connect proves the pull
// credentials work before any machine is woken.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Ref is a parsed image reference.
type Ref struct {
	// Host is the registry, e.g. ghcr.io; Docker Hub's is
	// registry-1.docker.io.
	Host string
	// Repo is the repository path, e.g. visheshgubrani/clipmux.
	Repo string
	// Tag is the tag, empty when the reference names a digest.
	Tag string
	// Digest is "sha256:…" when the reference names one.
	Digest string
}

// Pinned is the reference by digest, as a provider pulls it.
func (r Ref) Pinned() string {
	host := r.Host
	if host == "registry-1.docker.io" {
		host = "docker.io"
	}
	return host + "/" + r.Repo + "@" + r.Digest
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Parse reads ref the way docker does: a first path segment with a dot, a
// colon or "localhost" is a registry host; otherwise the image is on
// Docker Hub. A reference without a tag or digest means "latest".
func Parse(ref string) (Ref, error) {
	var r Ref
	name := ref
	if i := strings.Index(name, "@"); i >= 0 {
		name, r.Digest = name[:i], name[i+1:]
		if !digestRE.MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("image %q: the digest must be sha256:<64 hex>", ref)
		}
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, r.Tag = name[:i], name[i+1:]
	}
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Host, r.Repo = first, rest
	} else {
		r.Host, r.Repo = "registry-1.docker.io", name
		if !strings.Contains(name, "/") {
			r.Repo = "library/" + name
		}
	}
	if r.Repo == "" || strings.ContainsAny(r.Repo, " \t") {
		return Ref{}, fmt.Errorf("image %q has no repository", ref)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Resolver asks registries for digests.
type Resolver struct {
	HTTP *http.Client
}

// NewResolver bounds every call.
func NewResolver() *Resolver { return &Resolver{HTTP: &http.Client{Timeout: 30 * time.Second}} }

// accept lists the manifest types a tag may point at, image indexes first,
// so a multi-platform tag resolves to its index's digest as docker does.
var accept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// ErrDenied means the image can't be read with the credentials given (or
// anonymously): it is private to others, or it doesn't exist for them, since
// registries don't tell the two apart.
var ErrDenied = errors.New("registry: the image can't be pulled with these credentials")

// ErrBadCredentials means the registry refused the username and token
// themselves, before any image was asked for.
var ErrBadCredentials = errors.New("registry: the credentials were refused")

// ErrNotFound means credentials the registry accepted found no such image or
// tag.
var ErrNotFound = errors.New("registry: no such image or tag")

// Resolve pins ref to its digest, pulling the manifest as username and
// password would (anonymously when both are empty). A reference that names
// a digest is still checked, because Connect uses this to prove access.
func (r *Resolver) Resolve(ctx context.Context, ref, username, password string) (Ref, error) {
	p, err := Parse(ref)
	if err != nil {
		return Ref{}, err
	}
	reference := p.Tag
	if p.Digest != "" {
		reference = p.Digest
	}
	u := "https://" + p.Host + "/v2/" + p.Repo + "/manifests/" + url.PathEscape(reference)
	res, err := r.get(ctx, u, p, username, password)
	if err != nil {
		return Ref{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return Ref{}, fmt.Errorf("registry %s: %w", p.Host, err)
	}
	// The registry's header names the digest of what it served; without
	// one, the digest is that of the body, which is what pulls use.
	digest := res.Header.Get("Docker-Content-Digest")
	if !digestRE.MatchString(digest) {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if p.Digest != "" && p.Digest != digest {
		return Ref{}, fmt.Errorf("registry %s served %s for %s", p.Host, digest, p.Digest)
	}
	p.Digest = digest
	return p, nil
}

// get fetches the manifest, answering the registry's auth challenge once:
// a Bearer challenge is a token request (with basic auth when there are
// credentials), a Basic one is basic auth on the manifest itself.
func (r *Resolver) get(ctx context.Context, u string, p Ref, username, password string) (*http.Response, error) {
	do := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := r.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", p.Host, err)
		}
		return res, nil
	}
	ok := func(res *http.Response, err error) (*http.Response, error) { return check(res, err, p, username != "") }
	res, err := do("")
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return check(res, err, p, false)
	}
	challenge := res.Header.Get("WWW-Authenticate")
	res.Body.Close()
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "bearer":
		token, err := r.token(ctx, params, p, username, password)
		if err != nil {
			return nil, err
		}
		return ok(do("Bearer " + token))
	case "basic":
		if username == "" {
			return nil, ErrDenied
		}
		req := &http.Request{Header: http.Header{}}
		req.SetBasicAuth(username, password)
		return ok(do(req.Header.Get("Authorization")))
	}
	return nil, fmt.Errorf("registry %s: unsupported auth challenge %q", p.Host, scheme)
}

// check maps the manifest's status. A 404 means no such tag only to
// credentials the registry accepted; anonymously it may be a private image.
func check(res *http.Response, err error, p Ref, authed bool) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusOK:
		return res, nil
	case res.StatusCode == http.StatusNotFound && authed:
		res.Body.Close()
		return nil, ErrNotFound
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound:
		res.Body.Close()
		return nil, ErrDenied
	}
	res.Body.Close()
	return nil, fmt.Errorf("registry %s: HTTP %d", p.Host, res.StatusCode)
}

func (r *Resolver) token(ctx context.Context, params map[string]string, p Ref, username, password string) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" && realm.Host != p.Host {
		// A realm elsewhere over plain http would leak the credentials.
		return "", fmt.Errorf("registry %s: refusing token realm %q", p.Host, params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + p.Repo + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	res, err := r.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry %s token: %w", p.Host, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		if username != "" {
			return "", ErrBadCredentials
		}
		return "", ErrDenied
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s token: HTTP %d", p.Host, res.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("registry %s token: %w", p.Host, err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", fmt.Errorf("registry %s token: empty", p.Host)
	}
	return t.Token, nil
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		var kv string
		rest = strings.TrimLeft(rest, " ,")
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			kv, rest = after[1:1+end], after[2+end:]
		} else {
			kv, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(k))] = kv
	}
	return scheme, params
}
