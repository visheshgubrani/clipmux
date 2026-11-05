// Package web embeds the built dashboard (a Vite + React SPA from P1-T10;
// until then a placeholder page). The dashboard is an API client with no
// private path into the server.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// Dist returns the built dashboard rooted at its index.html.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed directive guarantees dist exists
	}
	return sub
}
