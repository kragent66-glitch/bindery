package main

import (
	"io/fs"
	"net/http"
	"path"
)

// staticContentTypes covers files in the embedded frontend whose extension
// Go's mime table does not know. Without an entry http.FileServer sniffs the
// body, and a web app manifest comes back as text/plain, which the
// X-Content-Type-Options: nosniff header then pins.
var staticContentTypes = map[string]string{
	".webmanifest": "application/manifest+json",
}

// spaHandler serves the embedded frontend: index.html (with the <base> tag
// already injected) for the root and for any path that is not a real file,
// so client side routes survive a reload, and the file itself otherwise.
func spaHandler(distFS fs.FS, indexHTML []byte) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(distFS))
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write(indexHTML)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path[1:]
		if p == "" || p == "index.html" {
			serveIndex(w)
			return
		}
		if _, err := fs.Stat(distFS, p); err == nil {
			if ct, ok := staticContentTypes[path.Ext(p)]; ok {
				// http.FileServer keeps a Content-Type that is already set.
				w.Header().Set("Content-Type", ct)
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// SPA fallback: unknown paths render the app shell.
		serveIndex(w)
	}
}
