// Package web serves the browser UI that is built into the binary.
//
// An embed directive cannot reach outside its package, so the UI is embedded from dist/
// here rather than from web/dist. Two things live in dist/:
//
//   - dist/index.html is a placeholder page. It is tracked in git and never overwritten,
//     so the package always compiles and a plain `go build` gives a working daemon.
//   - dist/app/ is the built Svelte app. `make web` copies web/dist there; it is ignored
//     by git. When dist/app/index.html is in the binary it is served instead of the
//     placeholder.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the built app when the binary has one, and the placeholder page otherwise.
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the path is a constant; this cannot fail
	}
	if app, err := fs.Sub(root, "app"); err == nil {
		if _, err := fs.Stat(app, "index.html"); err == nil {
			root = app
		}
	}
	return handler(root)
}

// handler serves root as a single-page app: a file when the path names one, index.html
// for any other path without a file extension (a client-side route), and 404 for a
// missing file. It is mounted under the API's routes and never answers for them.
func handler(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if info, err := fs.Stat(root, name); err != nil || info.IsDir() { // "" (the path /) is an error too
			if path.Ext(name) != "" {
				http.NotFound(w, r) // a missing asset must not come back as HTML
				return
			}
			name = "index.html"
		}
		h := w.Header()
		switch {
		case strings.HasPrefix(name, "assets/"):
			// Vite puts a content hash in every name under assets/, so a name never changes meaning.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			// index.html names the current assets; the browser must ask for it every time.
			h.Set("Cache-Control", "no-cache")
		}
		if path.Ext(name) == ".webmanifest" { // not in Go's built-in table, and the image has no /etc/mime.types
			h.Set("Content-Type", "application/manifest+json")
		}
		if name == "index.html" {
			// ServeFileFS redirects a request path ending in /index.html to its directory;
			// serving through a copy of the request with path "/" avoids that.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		http.ServeFileFS(w, r, root, name) // #nosec G703 -- name is cleaned, was found in root by fs.Stat, and root is the embedded tree
	})
}
