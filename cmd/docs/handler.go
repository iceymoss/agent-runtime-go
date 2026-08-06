package main

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

func spaHandler(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if requested == "." || requested == "" {
			requested = "index.html"
		}
		resolved := requested
		if info, err := fs.Stat(assets, resolved); err != nil || info.IsDir() {
			resolved = strings.TrimSuffix(requested, "/") + ".html"
		}
		if info, err := fs.Stat(assets, resolved); err == nil && !info.IsDir() {
			if strings.HasPrefix(requested, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else if strings.HasSuffix(resolved, ".html") {
				w.Header().Set("Cache-Control", "no-cache")
			}
			if resolved != requested {
				r.URL.Path = "/" + resolved
			}
			files.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}
