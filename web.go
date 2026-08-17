package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// The web client is compiled into the binary, so a single file is all you need
// to ship the whole thing.
//
//go:embed web
var webFS embed.FS

func webClientHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("embedded web client is missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client is a single page; anything unknown is not a route.
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			if _, err := fs.Stat(sub, r.URL.Path[1:]); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
