package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded Vue SPA with fallback to index.html for
// client-side routing. If the dist directory is empty (e.g. you forgot to
// run `make web`), it falls back to a minimal placeholder page.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return placeholderHandler()
	}
	// If dist is empty (only the embed root exists), placeholder.
	entries, _ := fs.ReadDir(sub, ".")
	if len(entries) == 0 {
		return placeholderHandler()
	}

	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return placeholderHandler()
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		clean := path.Clean(r.URL.Path)
		if clean == "/" || clean == "" {
			serveIndex(w, indexBytes)
			return
		}
		// Check if file exists in embedded FS.
		rel := strings.TrimPrefix(clean, "/")
		if _, err := fs.Stat(sub, rel); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback.
		serveIndex(w, indexBytes)
	})
}

func serveIndex(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func placeholderHandler() http.Handler {
	const page = `<!doctype html>
<html><head><meta charset="utf-8"><title>Linda_Cam</title></head>
<body style="font-family:sans-serif;background:#111;color:#eee;padding:2rem">
<h1>Linda_Cam</h1>
<p>Frontend has not been built yet. Run <code>make web</code> and rebuild the binary.</p>
</body></html>`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
}
