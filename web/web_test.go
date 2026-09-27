package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	get := func(path, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	root := get("/", "")
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "Internet Monitor") {
		t.Fatalf("/ = %d", root.Code)
	}
	if root.Header().Get("Content-Security-Policy") == "" || root.Header().Get("ETag") == "" {
		t.Fatalf("missing headers: %v", root.Header())
	}
	if got := get("/", root.Header().Get("ETag")); got.Code != http.StatusNotModified {
		t.Fatalf("revalidation = %d, want 304", got.Code)
	}
	for _, p := range []string{"/app.js", "/app.css", "/vendor/uPlot.iife.min.js"} {
		if r := get(p, ""); r.Code != http.StatusOK || r.Body.Len() == 0 {
			t.Errorf("%s = %d", p, r.Code)
		}
	}
	if ct := get("/app.js", "").Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js content type = %q", ct)
	}
	if r := get("/../go.mod", ""); r.Code != http.StatusNotFound {
		t.Errorf("path traversal = %d, want 404", r.Code)
	}
}
