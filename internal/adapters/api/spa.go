package api

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Security headers of the web UI. The SPA loads only same-origin scripts,
// styles (Radix positions popovers with inline styles), fonts and images,
// talks only to this origin, and may not be framed.
const (
	uiContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	uiReferrerPolicy = "same-origin"

	// Vite names files under assets/ by content hash, so they never change.
	cacheImmutable = "public, max-age=31536000, immutable"
	// index.html and the other unhashed files are revalidated on every load.
	cacheRevalidate = "no-cache"
)

// spaHandler serves the built web UI. See newSPAHandler.
type spaHandler struct {
	files fs.FS
	log   *slog.Logger
}

// newSPAHandler serves the embedded web UI (Deps.UI) for every path the API
// does not claim: existing files directly (/assets/* as immutable), unknown
// GET paths as index.html (client-side routes), with the security headers
// of the UI (Content-Security-Policy, X-Content-Type-Options,
// Referrer-Policy). Paths under /syncr.v1. and /healthz are never answered
// with the UI, so an unmounted service still looks absent (404) to clients.
func newSPAHandler(files fs.FS, logger *slog.Logger) http.Handler {
	return &spaHandler{files: files, log: logger}
}

// ServeHTTP implements http.Handler.
func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isAPIPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", uiReferrerPolicy)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hdr.Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	hdr.Set("Content-Security-Policy", uiContentSecurityPolicy)

	// Cleaning a rooted path removes every ".." segment; fs.ValidPath then
	// guarantees the name stays inside the UI's file system.
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "index.html" && fs.ValidPath(name) && h.isFile(name) {
		if strings.HasPrefix(name, "assets/") {
			hdr.Set("Cache-Control", cacheImmutable)
		} else {
			hdr.Set("Cache-Control", cacheRevalidate)
		}
		http.ServeFileFS(w, r, h.files, name) //nolint:gosec // G703: name is a cleaned fs.ValidPath inside an fs.FS
		return
	}
	// A missing hashed asset is a stale page asking for an old build: a
	// 404 makes the browser fail loudly instead of parsing HTML as script.
	if strings.HasPrefix(name, "assets/") {
		hdr.Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

// serveIndex answers with index.html, the entry of every client route.
func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.files.Open("index.html")
	if err != nil {
		h.log.ErrorContext(r.Context(), "web ui has no index.html", "error", err)
		http.Error(w, "the web UI is not available", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		h.log.ErrorContext(r.Context(), "web ui index.html", "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		h.log.ErrorContext(r.Context(), "web ui index.html is not seekable")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", cacheRevalidate)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// ServeContent (not ServeFileFS) because the request path is a client
	// route, and ServeFileFS would redirect a path ending in /index.html.
	http.ServeContent(w, r, "index.html", st.ModTime(), rs)
}

// isFile reports whether name is a regular file in the UI (directories
// are client routes, never listings).
func (h *spaHandler) isFile(name string) bool {
	st, err := fs.Stat(h.files, name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.log.Warn("web ui stat failed", "path", name, "error", err)
		}
		return false
	}
	return st.Mode().IsRegular()
}

// isAPIPath reports whether a path belongs to the API, which the UI must
// never shadow: Connect procedures (/syncr.v1.<Service>/<Method>) and the
// health endpoint.
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/syncr.v1.") || p == "/healthz" || strings.HasPrefix(p, "/healthz/")
}
