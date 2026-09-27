// Package web embeds the dashboard's static files.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var files embed.FS

type asset struct {
	body  []byte
	etag  string
	ctype string
}

// Handler serves the dashboard. Files are revalidated on every load with a
// content-hash ETag, so upgrades take effect without stale caches.
func Handler() http.Handler {
	assets := map[string]asset{}
	static, _ := fs.Sub(files, "static")
	fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(static, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		ctype := mime.TypeByExtension(path.Ext(p))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		assets["/"+p] = asset{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ctype}
		return nil
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if p == "/" {
			p = "/index.html"
		}
		a, ok := assets[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.ctype)
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", a.etag)
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasSuffix(p, ".html") {
			h.Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		}
		if match := r.Header.Get("If-None-Match"); match != "" && match == a.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write(a.body)
	})
}
