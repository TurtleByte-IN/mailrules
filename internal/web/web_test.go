package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return rec
}

func TestHandler(t *testing.T) {
	const immutable, revalidate = "public, max-age=31536000, immutable", "no-cache"
	h := handler(fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><title>app</title>")},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
		"icon.svg":                {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		"manifest.webmanifest":    {Data: []byte("{}")},
	})
	for _, tc := range []struct {
		method, path string
		status       int
		contentType  string
		cache        string
		body         string
	}{
		{"GET", "/", 200, "text/html", revalidate, "<title>app</title>"},
		{"GET", "/index.html", 200, "text/html", revalidate, "<title>app</title>"},
		{"GET", "/rules/12", 200, "text/html", revalidate, "<title>app</title>"}, // a client-side route
		{"GET", "/assets", 200, "text/html", revalidate, "<title>app</title>"},   // a directory is not a file
		{"GET", "/assets/index-abc123.js", 200, "text/javascript", immutable, "console.log"},
		{"GET", "/assets/index-abc123.css", 200, "text/css", immutable, "body{}"},
		{"GET", "/icon.svg", 200, "image/svg+xml", revalidate, "<svg"},
		{"GET", "/manifest.webmanifest", 200, "application/manifest+json", revalidate, "{}"},
		{"GET", "/assets/gone-000000.js", 404, "", "", ""}, // a missing asset is not index.html
		{"GET", "/../../etc/passwd", 200, "text/html", revalidate, "<title>app</title>"},
		{"HEAD", "/", 200, "text/html", revalidate, ""},
		{"POST", "/", 405, "", "", ""},
	} {
		rec := get(t, h, tc.method, tc.path)
		if rec.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.status)
			continue
		}
		if tc.status != 200 {
			if strings.Contains(rec.Body.String(), "<title>app</title>") {
				t.Errorf("%s %s answered with index.html", tc.method, tc.path)
			}
			continue
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Errorf("%s %s Content-Type = %q, want %q", tc.method, tc.path, got, tc.contentType)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s %s Cache-Control = %q, want %q", tc.method, tc.path, got, tc.cache)
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s %s body = %q, want it to contain %q", tc.method, tc.path, rec.Body.String(), tc.body)
		}
	}
}

// The placeholder is served under Content-Security-Policy: default-src 'self', which
// refuses inline styles and scripts and anything from another origin.
func TestPlaceholderIsSelfContained(t *testing.T) {
	page, err := dist.ReadFile("dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "not built into this binary") {
		t.Error("the placeholder does not say the UI is missing")
	}
	for _, banned := range []string{"<style", "style=", "<script", "<link", "<img", "http://", "https://"} {
		if strings.Contains(string(page), banned) {
			t.Errorf("the placeholder contains %q", banned)
		}
	}
	if rec := get(t, Handler(), "GET", "/"); rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("GET / on the embedded UI = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
