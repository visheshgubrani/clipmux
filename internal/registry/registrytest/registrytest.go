// Package registrytest is a fake private container registry for tests
// (D058). It speaks the OCI distribution API's Bearer token flow, the way
// ghcr.io does, over TLS, and serves one manifest to one set of
// credentials.
package registrytest

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Username and Password are the only credentials the fake accepts.
const (
	Username = "visheshgubrani"
	Password = "ghp_TESTREGISTRYTOKEN"
	Repo     = "visheshgubrani/clipmux"
	Tag      = "sha-abc"
	token    = "registry-bearer-token"
)

// Manifest is what the fake serves for Repo:Tag.
var Manifest = []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)

// Registry is the fake. Client trusts its certificate.
type Registry struct {
	Host   string
	Client *http.Client
	// Digest is the manifest's digest.
	Digest string
}

// New starts the fake for the test.
func New(t *testing.T) *Registry {
	t.Helper()
	sum := sha256.Sum256(Manifest)
	r := &Registry{Digest: "sha256:" + hex.EncodeToString(sum[:])}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/token":
			user, pass, ok := req.BasicAuth()
			if !ok || user != Username || pass != Password || req.URL.Query().Get("scope") != "repository:"+Repo+":pull" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"token":"` + token + `"}`))
		case strings.HasPrefix(req.URL.Path, "/v2/"):
			if req.Header.Get("Authorization") != "Bearer "+token {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+r.Host+`/token",service="`+r.Host+`",scope="repository:`+Repo+`:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if req.URL.Path != "/v2/"+Repo+"/manifests/"+Tag && req.URL.Path != "/v2/"+Repo+"/manifests/"+r.Digest {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", r.Digest)
			_, _ = w.Write(Manifest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	r.Host = strings.TrimPrefix(srv.URL, "https://")
	r.Client = srv.Client()
	return r
}

// Image is Repo:Tag on the fake.
func (r *Registry) Image() string { return r.Host + "/" + Repo + ":" + Tag }

// Pinned is the image by digest, as Connect stores it.
func (r *Registry) Pinned() string { return r.Host + "/" + Repo + "@" + r.Digest }
