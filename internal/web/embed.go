// Package web serves the dashboard, a Vite + React SPA built into dist/ by
// `make web` (the image does it too). The dashboard is an API client with no
// private path into the server. A binary built without it serves a
// placeholder page.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist placeholder.html
var files embed.FS

// Dist returns the built dashboard, or a placeholder when there is none.
func Dist() fs.FS {
	sub, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err) // the embed directive guarantees dist exists
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return placeholder{}
	}
	return sub
}

type placeholder struct{}

func (placeholder) Open(name string) (fs.File, error) {
	if name == "index.html" {
		return files.Open("placeholder.html")
	}
	return nil, fs.ErrNotExist
}

// reserved are the server's own paths: a typo under them is a JSON 404,
// never the dashboard.
var reserved = []string{"/v1/", "/v/", "/p/", "/_storage/", "/worker/", "/_tls/", "/healthz", "/readyz", "/metrics", "/debug/"}

// DefaultCSP is the dashboard's Content-Security-Policy when the server
// gives none: everything from its own origin only.
const DefaultCSP = "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// Handler serves the SPA from fsys: a real file as itself (hashed assets
// for a year), any other path as index.html, uncached, so a deep link
// loads the app. csp gives the page's Content-Security-Policy, which
// depends on the media URL and the storage buckets; nil uses DefaultCSP.
func Handler(fsys fs.FS, csp func(*http.Request) string) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range reserved {
			if strings.HasPrefix(r.URL.Path, p) || r.URL.Path == strings.TrimSuffix(p, "/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}` + "\n"))
				return
			}
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		policy := DefaultCSP
		if csp != nil {
			policy = csp(r)
		}
		h.Set("Content-Security-Policy", policy)
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if info, err := fs.Stat(fsys, name); name != "" && err == nil && !info.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}
