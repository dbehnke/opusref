// Package webassets serves the compiled OpusRef browser application.
package webassets

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var production embed.FS

// Handler returns a read-only handler for the production application.
func Handler() http.Handler {
	dist, err := fs.Sub(production, "dist")
	if err != nil {
		panic("webassets: embedded dist is missing")
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(dist, name); err == nil {
			files.ServeHTTP(response, request)
			return
		}
		if strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
			http.NotFound(response, request)
			return
		}
		request = request.Clone(request.Context())
		request.URL.Path = "/"
		files.ServeHTTP(response, request)
	})
}
