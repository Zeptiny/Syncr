package api

import (
	"io/fs"
	"log/slog"
	"net/http"
)

// newSPAHandler serves the embedded web UI (Deps.UI) for every path the API
// does not claim: existing files directly (/assets/* as immutable), unknown
// GET paths as index.html (client-side routes), with the security headers
// of the UI (Content-Security-Policy, X-Content-Type-Options,
// Referrer-Policy).
//
// m3: pending track D: until then it serves nothing (404).
func newSPAHandler(_ fs.FS, _ *slog.Logger) http.Handler {
	return http.NotFoundHandler()
}
