// Package api is the controller's inbound HTTP adapter: it serves the
// Connect API (proto/syncr/v1/api.proto) and health endpoints, translating
// between wire messages and application services. Later milestones add
// authentication middleware and the embedded web UI here.
//
// Allowed dependencies: the standard library, connect, the generated
// protobuf code, internal/domain, internal/app/... services and
// internal/platform packages. It must not import other adapters; the
// composition root (cmd/syncr-controller) wires them together.
package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// maxRequestBytes caps the size of a decoded API request message.
const maxRequestBytes = 4 << 20

// Deps are the collaborators of the API handler.
type Deps struct {
	// BuildInfo is reported by SystemService.GetInfo.
	BuildInfo version.Info
	// Logger receives handler panics and other server-side errors.
	Logger *slog.Logger
}

// NewHandler returns the controller's HTTP handler: the Connect services
// under their /syncr.v1.<Service>/ paths and GET /healthz.
func NewHandler(deps Deps) http.Handler {
	opts := []connect.HandlerOption{
		connect.WithRecover(recoverHandler(deps.Logger)),
		connect.WithReadMaxBytes(maxRequestBytes),
	}

	mux := http.NewServeMux()
	mux.Handle(syncrv1connect.NewSystemServiceHandler(NewSystemService(deps.BuildInfo), opts...))
	mux.HandleFunc("GET /healthz", healthz)
	return mux
}

// healthz reports liveness. It deliberately checks nothing beyond the
// process serving HTTP; readiness checks (database) arrive with the store.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, "ok\n")
}

// recoverHandler logs a handler panic and turns it into an Internal error.
func recoverHandler(logger *slog.Logger) func(context.Context, connect.Spec, http.Header, any) error {
	return func(ctx context.Context, spec connect.Spec, _ http.Header, recovered any) error {
		logger.ErrorContext(ctx, "api handler panicked", "procedure", spec.Procedure, "panic", recovered)
		return connect.NewError(connect.CodeInternal, nil)
	}
}
