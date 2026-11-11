package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
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

// uiContentType returns the Content-Type of a UI file from a fixed table
// of the extensions the built UI ships. The handler sets it itself instead
// of letting net/http consult mime.TypeByExtension, which reads the OS
// database (the registry on Windows) and can return a different or wrong
// type there; under nosniff a wrong type breaks module scripts and
// stylesheets. Unknown extensions are application/octet-stream, which a
// browser never runs or renders under nosniff.
func uiContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".webmanifest":
		return "application/manifest+json"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/vnd.microsoft.icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// compressible reports whether a UI file is of a type worth gzipping: text
// formats and SVG (fonts and the raster images are compressed already).
func compressible(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".js", ".mjs", ".css", ".json", ".map", ".webmanifest", ".txt", ".svg", ".ico":
		return true
	}
	return false
}

// minGzipSize is the smallest file worth a gzipped copy; smaller ones gain
// too little to be worth the decoding.
const minGzipSize = 256

// uiFile is what the handler computed for one UI file at startup.
type uiFile struct {
	// etag is the strong ETag of the file as stored (from its SHA-256).
	etag string
	// gz is its gzipped copy, nil unless the file is compressible, at
	// least minGzipSize bytes and smaller gzipped; gzETag is that copy's
	// ETag (another representation, so another ETag).
	gz     []byte
	gzETag string
}

// spaHandler serves the built web UI. See newSPAHandler.
type spaHandler struct {
	files fs.FS
	log   *slog.Logger
	// meta holds the uiFile of every regular file, by name; it is read-only
	// after newSPAHandler.
	meta map[string]uiFile
}

// newSPAHandler serves the embedded web UI (Deps.UI) for every path the API
// does not claim: existing files directly (/assets/* as immutable), unknown
// GET paths as index.html (client-side routes), with the security headers
// of the UI (Content-Security-Policy, X-Content-Type-Options,
// Referrer-Policy). Paths under /syncr.v1. and /healthz are never answered
// with the UI, so an unmounted service still looks absent (404) to clients.
//
// It reads every file once, here: each gets a strong ETag, and each
// compressible one (see compressible) a gzipped copy, served with
// Content-Encoding: gzip to requests whose Accept-Encoding allows gzip
// (Vary: Accept-Encoding on both variants). A file that cannot be read
// here is still served, without either.
func newSPAHandler(files fs.FS, logger *slog.Logger) http.Handler {
	return &spaHandler{files: files, log: logger, meta: prepareUI(files, logger)}
}

// prepareUI computes the uiFile of every regular file of files.
func prepareUI(files fs.FS, logger *slog.Logger) map[string]uiFile {
	meta := map[string]uiFile{}
	err := fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			logger.Warn("web ui: cannot read a directory", "path", name, "error", err)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := prepareFile(files, name)
		if err != nil {
			logger.Warn("web ui: cannot prepare a file; it is served without an ETag or compression", "path", name, "error", err)
			return nil
		}
		meta[name] = f
		return nil
	})
	if err != nil {
		logger.Warn("web ui: cannot list the files", "error", err)
	}
	return meta
}

// prepareFile hashes name and, if it is compressible, gzips it.
func prepareFile(files fs.FS, name string) (uiFile, error) {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return uiFile{}, err
	}
	sum := sha256.Sum256(data)
	tag := base64.RawURLEncoding.EncodeToString(sum[:18])
	out := uiFile{etag: `"` + tag + `"`}
	if !compressible(name) || len(data) < minGzipSize {
		return out, nil
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return uiFile{}, err
	}
	if _, err := zw.Write(data); err != nil {
		return uiFile{}, err
	}
	if err := zw.Close(); err != nil {
		return uiFile{}, err
	}
	if buf.Len() < len(data) {
		out.gz, out.gzETag = buf.Bytes(), `"`+tag+`-gzip"`
	}
	return out, nil
}

// acceptsGzip reports whether a request's Accept-Encoding allows a gzip
// response: gzip (or x-gzip) listed with a q-value above zero, or, when it
// is not listed, "*" with one. No header means identity only, as browsers
// always send one.
func acceptsGzip(h http.Header) bool {
	gz, star := -1.0, -1.0 // -1: not listed
	for _, v := range h.Values("Accept-Encoding") {
		for part := range strings.SplitSeq(v, ",") {
			coding, params, _ := strings.Cut(part, ";")
			q := 1.0
			for param := range strings.SplitSeq(params, ";") {
				k, val, ok := strings.Cut(param, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
					continue
				}
				if q, ok = parseQ(strings.TrimSpace(val)); !ok {
					q = 0
				}
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip", "x-gzip":
				gz = max(gz, q)
			case "*":
				star = max(star, q)
			}
		}
	}
	if gz >= 0 {
		return gz > 0
	}
	return star > 0
}

// parseQ parses a q-value (0 to 1).
func parseQ(s string) (float64, bool) {
	q, err := strconv.ParseFloat(s, 64)
	if err != nil || q < 0 || q > 1 {
		return 0, false
	}
	return q, true
}

// serveGzip answers with the gzipped copy of name when it has one and the
// request accepts gzip, and reports whether it did. Otherwise it only sets
// the headers of the stored representation (its ETag, and Vary when a
// gzipped copy exists), for the caller to serve it.
func (h *spaHandler) serveGzip(w http.ResponseWriter, r *http.Request, name string) bool {
	f, ok := h.meta[name]
	if !ok {
		return false
	}
	hdr := w.Header()
	if f.gz != nil {
		hdr.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header) {
			serveEncoded(w, r, f.gzETag, f.gz)
			return true
		}
	}
	hdr.Set("ETag", f.etag)
	return false
}

// serveEncoded answers a GET or HEAD with the gzipped body and its ETag:
// 304 Not Modified when If-None-Match names it (or "*"), otherwise 200 with
// Content-Encoding: gzip and the body's Content-Length. It offers no ranges
// (http.ServeContent would, but leaves Content-Length out for an encoded
// body), so a Range header gets the whole body.
func serveEncoded(w http.ResponseWriter, r *http.Request, etag string, body []byte) {
	hdr := w.Header()
	hdr.Set("ETag", etag)
	if noneMatch(r.Header.Values("If-None-Match"), etag) {
		// As net/http does for a 304: no body, so no body headers.
		hdr.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hdr.Set("Content-Encoding", "gzip")
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// noneMatch reports whether If-None-Match values match etag: "*", or etag
// among the listed entity tags by weak comparison (a W/ prefix is ignored).
func noneMatch(values []string, etag string) bool {
	for _, v := range values {
		for tag := range strings.SplitSeq(v, ",") {
			tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
			if tag == "*" || tag == etag {
				return true
			}
		}
	}
	return false
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
		// Set before serving, so ServeFileFS neither sniffs nor overrides it.
		hdr.Set("Content-Type", uiContentType(name))
		if h.serveGzip(w, r, name) {
			return
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
	w.Header().Set("Cache-Control", cacheRevalidate)
	w.Header().Set("Content-Type", uiContentType("index.html"))
	if h.serveGzip(w, r, "index.html") {
		return
	}
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
