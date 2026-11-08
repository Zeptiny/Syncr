package api

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	}
}

func TestSPA(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(NewHandler(Deps{Logger: logging.Discard(), UI: testUI()}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name, method, path string
		wantStatus         int
		wantBody           string // substring; "" skips
		wantCache          string // "" means the header must be absent
		wantType           string // prefix; "" skips
		wantCSP            bool
	}{
		{"root", "GET", "/", 200, testIndex, cacheRevalidate, "text/html", true},
		{"index file", "GET", "/index.html", 200, testIndex, cacheRevalidate, "text/html", true},
		{"client route", "GET", "/jobs/0b9c8f1e-4a7d-4c55", 200, testIndex, cacheRevalidate, "text/html", true},
		{"client route with query", "GET", "/login?redirect=%2Fjobs", 200, testIndex, cacheRevalidate, "text/html", true},
		{"directory is a route", "GET", "/assets", 200, testIndex, cacheRevalidate, "text/html", true},
		{"traversal stays inside", "GET", "/../../etc/passwd", 200, testIndex, cacheRevalidate, "text/html", true},
		{"head", "HEAD", "/remotes", 200, "", cacheRevalidate, "text/html", true},
		{"hashed asset", "GET", "/assets/index-abc123.js", 200, "console.log(1)", cacheImmutable, "text/javascript", true},
		{"hashed stylesheet", "GET", "/assets/index-abc123.css", 200, "body{}", cacheImmutable, "text/css", true},
		{"missing asset", "GET", "/assets/index-old999.js", 404, "", "no-store", "", true},
		{"unhashed file", "GET", "/favicon.svg", 200, "<svg/>", cacheRevalidate, "image/svg+xml", true},
		{"post", "POST", "/jobs", 405, "", "", "", false},
		{"unmounted service", "POST", "/syncr.v1.UserService/ListUsers", 404, "", "", "", false},
		{"unknown procedure path", "GET", "/syncr.v1.Nope/Get", 404, "", "", "", false},
		{"health", "GET", "/healthz", 200, "ok", "no-store", "text/plain", false},
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
			if tt.wantType != "" && !strings.HasPrefix(resp.Header.Get("Content-Type"), tt.wantType) {
				t.Errorf("Content-Type = %q, want %s…", resp.Header.Get("Content-Type"), tt.wantType)
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
