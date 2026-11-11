package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

const testIndex = "<!doctype html><title>Syncr</title>"

func testUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte(testIndex)},
		"favicon.svg":             {Data: []byte("<svg/>")},
		"theme-init.js":           {Data: []byte("void 0")},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
		"assets/plex-sans.woff2":  {Data: []byte("wOF2")},
		"assets/plex-mono.woff":   {Data: []byte("wOFF")},
		"assets/data.bin":         {Data: []byte("<script>alert(1)</script>")},
		"assets/upper-ABC.JS":     {Data: []byte("void 0")},
		"robots.txt":              {Data: []byte("User-agent: *")},
	}
}

func TestSPA(t *testing.T) {
	t.Parallel()
	const (
		html = "text/html; charset=utf-8"
		js   = "text/javascript; charset=utf-8"
	)
	srv := httptest.NewServer(NewHandler(Deps{Logger: logging.Discard(), UI: testUI()}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name, method, path string
		wantStatus         int
		wantBody           string // substring; "" skips
		wantCache          string // "" means the header must be absent
		wantType           string // exact; "" skips
		wantCSP            bool
	}{
		{"root", "GET", "/", 200, testIndex, cacheRevalidate, html, true},
		{"index file", "GET", "/index.html", 200, testIndex, cacheRevalidate, html, true},
		{"client route", "GET", "/jobs/0b9c8f1e-4a7d-4c55", 200, testIndex, cacheRevalidate, html, true},
		{"client route with query", "GET", "/login?redirect=%2Fjobs", 200, testIndex, cacheRevalidate, html, true},
		{"directory is a route", "GET", "/assets", 200, testIndex, cacheRevalidate, html, true},
		{"traversal stays inside", "GET", "/../../etc/passwd", 200, testIndex, cacheRevalidate, html, true},
		{"head", "HEAD", "/remotes", 200, "", cacheRevalidate, html, true},
		{"hashed asset", "GET", "/assets/index-abc123.js", 200, "console.log(1)", cacheImmutable, js, true},
		{"hashed stylesheet", "GET", "/assets/index-abc123.css", 200, "body{}", cacheImmutable, "text/css; charset=utf-8", true},
		{"font woff2", "GET", "/assets/plex-sans.woff2", 200, "wOF2", cacheImmutable, "font/woff2", true},
		{"font woff", "GET", "/assets/plex-mono.woff", 200, "wOFF", cacheImmutable, "font/woff", true},
		{"extension case", "GET", "/assets/upper-ABC.JS", 200, "void 0", cacheImmutable, js, true},
		{"unknown extension", "GET", "/assets/data.bin", 200, "<script>", cacheImmutable, "application/octet-stream", true},
		{"missing asset", "GET", "/assets/index-old999.js", 404, "", "no-store", "", true},
		{"unhashed file", "GET", "/favicon.svg", 200, "<svg/>", cacheRevalidate, "image/svg+xml", true},
		{"unhashed script", "GET", "/theme-init.js", 200, "void 0", cacheRevalidate, js, true},
		{"unhashed text", "GET", "/robots.txt", 200, "User-agent", cacheRevalidate, "text/plain; charset=utf-8", true},
		{"post", "POST", "/jobs", 405, "", "", "", false},
		{"unmounted service", "POST", "/syncr.v1.UserService/ListUsers", 404, "", "", "", false},
		{"unknown procedure path", "GET", "/syncr.v1.Nope/Get", 404, "", "", "", false},
		{"health", "GET", "/healthz", 200, "ok", "no-store", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, tt.wantStatus, body)
			}
			if tt.wantBody != "" && !strings.Contains(string(body), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", body, tt.wantBody)
			}
			if tt.wantStatus == 404 && strings.Contains(string(body), testIndex) {
				t.Errorf("404 served the UI: %q", body)
			}
			if got := resp.Header.Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, tt.wantCache)
			}
			if tt.wantType != "" && resp.Header.Get("Content-Type") != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", resp.Header.Get("Content-Type"), tt.wantType)
			}
			if got := resp.Header.Get("Content-Security-Policy"); (got == uiContentSecurityPolicy) != tt.wantCSP {
				t.Errorf("Content-Security-Policy = %q, want set: %v", got, tt.wantCSP)
			}
			if tt.wantCSP {
				if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
					t.Errorf("X-Content-Type-Options = %q", got)
				}
				if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
					t.Errorf("Referrer-Policy = %q", got)
				}
			}
			if tt.method == "POST" && tt.wantStatus == 405 && resp.Header.Get("Allow") != "GET, HEAD" {
				t.Errorf("Allow = %q", resp.Header.Get("Allow"))
			}
		})
	}
}

func TestSPAHeadHasNoBody(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	newSPAHandler(testUI(), logging.Discard()).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/jobs", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD = %d with %d body bytes, want 200 and none", rec.Code, rec.Body.Len())
	}
}

func TestSPAWithoutIndex(t *testing.T) {
	t.Parallel()
	ui := testUI()
	delete(ui, "index.html")
	rec := httptest.NewRecorder()
	newSPAHandler(ui, logging.Discard()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jobs", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestNoUIMountsNothing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewHandler(Deps{Logger: logging.Discard()}))
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Get(srv.URL + "/jobs")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 without Deps.UI", resp.StatusCode)
	}
}

// TestUIContentType pins the whole table, so the served types never depend
// on the OS MIME database.
func TestUIContentType(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"index.html":              "text/html; charset=utf-8",
		"assets/index-abc.js":     "text/javascript; charset=utf-8",
		"assets/worker.mjs":       "text/javascript; charset=utf-8",
		"assets/index-abc.css":    "text/css; charset=utf-8",
		"assets/index-abc.js.map": "application/json",
		"data.json":               "application/json",
		"site.webmanifest":        "application/manifest+json",
		"robots.txt":              "text/plain; charset=utf-8",
		"favicon.svg":             "image/svg+xml",
		"logo.png":                "image/png",
		"logo.webp":               "image/webp",
		"favicon.ico":             "image/vnd.microsoft.icon",
		"assets/plex.woff":        "font/woff",
		"assets/plex.woff2":       "font/woff2",
		"assets/INDEX.CSS":        "text/css; charset=utf-8",
		"assets/blob.wasm":        "application/octet-stream",
		"LICENSE":                 "application/octet-stream",
		"assets/archive.tar.gz":   "application/octet-stream",
	} {
		if got := uiContentType(name); got != want {
			t.Errorf("uiContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestSPAGzip: a compressible file is served gzipped only to requests that
// accept gzip, with its own Content-Length and ETag and Vary on both
// variants; other files and the API are never gzipped by the UI handler.
func TestSPAGzip(t *testing.T) {
	t.Parallel()
	bigJS := strings.Repeat("console.log('syncr');\n", 200)
	bigIndex := testIndex + strings.Repeat("<!-- padding -->", 50)
	ui := testUI()
	ui["index.html"] = &fstest.MapFile{Data: []byte(bigIndex)}
	ui["assets/app-1.js"] = &fstest.MapFile{Data: []byte(bigJS)}
	ui["assets/font-1.woff2"] = &fstest.MapFile{Data: []byte(strings.Repeat("wOF2", 200))}
	srv := httptest.NewServer(NewHandler(Deps{Logger: logging.Discard(), UI: ui}))
	t.Cleanup(srv.Close)
	// A client that neither asks for nor decodes gzip by itself.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	t.Cleanup(client.CloseIdleConnections)
	// reply is what get received (its body read and closed).
	type reply struct {
		StatusCode int
		Header     http.Header
	}
	get := func(t *testing.T, method, p string, hdr ...string) (reply, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return reply{resp.StatusCode, resp.Header}, body
	}
	gunzip := func(t *testing.T, b []byte) string {
		t.Helper()
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	for _, tt := range []struct{ path, want, typ string }{
		{"/assets/app-1.js", bigJS, "text/javascript; charset=utf-8"},
		{"/index.html", bigIndex, "text/html; charset=utf-8"},
		{"/jobs/some-route", bigIndex, "text/html; charset=utf-8"},
	} {
		plain, plainBody := get(t, "GET", tt.path)
		zipped, zippedBody := get(t, "GET", tt.path, "Accept-Encoding", "br;q=1, gzip;q=0.8")
		if plain.StatusCode != http.StatusOK || plain.Header.Get("Content-Encoding") != "" || string(plainBody) != tt.want {
			t.Errorf("%s without gzip: %d %q, %d bytes", tt.path, plain.StatusCode, plain.Header.Get("Content-Encoding"), len(plainBody))
		}
		if zipped.StatusCode != http.StatusOK || zipped.Header.Get("Content-Encoding") != "gzip" || gunzip(t, zippedBody) != tt.want {
			t.Fatalf("%s with gzip: %d %q", tt.path, zipped.StatusCode, zipped.Header.Get("Content-Encoding"))
		}
		if got := zipped.Header.Get("Content-Length"); got != strconv.Itoa(len(zippedBody)) || len(zippedBody) >= len(tt.want) {
			t.Errorf("%s: Content-Length %s for %d gzipped bytes of %d", tt.path, got, len(zippedBody), len(tt.want))
		}
		for _, resp := range []reply{plain, zipped} {
			if resp.Header.Get("Vary") != "Accept-Encoding" || resp.Header.Get("Content-Type") != tt.typ ||
				resp.Header.Get("Content-Security-Policy") != uiContentSecurityPolicy || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s: headers %v", tt.path, resp.Header)
			}
		}
		pe, ze := plain.Header.Get("ETag"), zipped.Header.Get("ETag")
		if pe == "" || ze == "" || pe == ze {
			t.Errorf("%s: ETags %q and %q, want two different ones", tt.path, pe, ze)
		}
		// Each variant revalidates against its own ETag only.
		if resp, body := get(t, "GET", tt.path, "Accept-Encoding", "gzip", "If-None-Match", ze); resp.StatusCode != http.StatusNotModified || len(body) != 0 || resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: gzip revalidation = %d, %q", tt.path, resp.StatusCode, resp.Header.Get("Content-Encoding"))
		}
		if resp, _ := get(t, "GET", tt.path, "If-None-Match", pe); resp.StatusCode != http.StatusNotModified {
			t.Errorf("%s: plain revalidation = %d", tt.path, resp.StatusCode)
		}
		if resp, body := get(t, "GET", tt.path, "Accept-Encoding", "gzip", "If-None-Match", pe); resp.StatusCode != http.StatusOK || gunzip(t, body) != tt.want {
			t.Errorf("%s: the plain ETag revalidated the gzipped copy: %d", tt.path, resp.StatusCode)
		}
		if resp, body := get(t, "GET", tt.path, "Accept-Encoding", "gzip", "If-None-Match", `W/"other", `+ze); resp.StatusCode != http.StatusNotModified || len(body) != 0 {
			t.Errorf("%s: revalidation with a list = %d", tt.path, resp.StatusCode)
		}
		// No ranges of the gzipped copy: the whole of it.
		if resp, body := get(t, "GET", tt.path, "Accept-Encoding", "gzip", "Range", "bytes=0-9"); resp.StatusCode != http.StatusOK || gunzip(t, body) != tt.want ||
			resp.Header.Get("Accept-Ranges") != "" {
			t.Errorf("%s: range of the gzipped copy = %d, Accept-Ranges %q", tt.path, resp.StatusCode, resp.Header.Get("Accept-Ranges"))
		}
		if resp, body := get(t, "HEAD", tt.path, "Accept-Encoding", "gzip"); resp.StatusCode != http.StatusOK || len(body) != 0 ||
			resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Length") != strconv.Itoa(len(zippedBody)) {
			t.Errorf("%s: HEAD = %d %v", tt.path, resp.StatusCode, resp.Header)
		}
	}

	// Accept-Encoding values that do not allow gzip.
	for _, ae := range []string{"", "identity", "br", "gzip;q=0", "*;q=0", "gzip;q=0, *", "gzip;q=x"} {
		if resp, _ := get(t, "GET", "/assets/app-1.js", "Accept-Encoding", ae); resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("Accept-Encoding %q got %q", ae, resp.Header.Get("Content-Encoding"))
		}
	}
	for _, ae := range []string{"gzip", "GZIP", "x-gzip", "*", "deflate, gzip;q=0.5", "*;q=0.1"} {
		if resp, _ := get(t, "GET", "/assets/app-1.js", "Accept-Encoding", ae); resp.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("Accept-Encoding %q not gzipped", ae)
		}
	}
	// Fonts, tiny files and the API are never gzipped by the UI handler.
	for _, p := range []string{"/assets/font-1.woff2", "/assets/index-abc123.js", "/healthz", "/syncr.v1.Nope/Get", "/assets/missing-1.js"} {
		resp, _ := get(t, "GET", p, "Accept-Encoding", "gzip")
		if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Vary") != "" {
			t.Errorf("%s: Content-Encoding %q, Vary %q", p, resp.Header.Get("Content-Encoding"), resp.Header.Get("Vary"))
		}
	}
}

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"gzip": true, "gzip, deflate, br, zstd": true, " GZip ;Q=0.5": true, "x-gzip": true, "*": true,
		"": false, "identity": false, "gzip;q=0": false, "gzip;q=0.0, *": false, "gzip;q=2": false, "*;q=0": false, "br;q=1": false,
	} {
		h := http.Header{}
		if v != "" {
			h.Set("Accept-Encoding", v)
		}
		if got := acceptsGzip(h); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", v, got, want)
		}
	}
}
