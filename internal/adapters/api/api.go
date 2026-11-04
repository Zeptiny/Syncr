// Package api is the controller's inbound HTTP adapter: it serves the
// Connect API (proto/syncr/v1/api.proto) and health endpoints, translating
// between wire messages and application services. Later milestones add the
// embedded web UI here.
//
// SystemService and /healthz are public. WorkerAdminService and JobService
// are protected by an Authenticator (Deps.Auth); until users and sessions
// arrive in Milestone 3 that is AdminTokenAuth, a shared bearer token from
// the controller's api.admin_token. Without an authenticator the protected
// services are not served at all.
//
// WatchJob streams extend their connection's read and write deadlines (to
// Deps.WatchMaxDuration plus a grace period), so the composition root may set
// http.Server ReadTimeout and WriteTimeout for ordinary calls without cutting
// streams short.
//
// Allowed dependencies: the standard library, connect, the generated
// protobuf code, internal/domain, internal/app/... services and
// internal/platform packages. It must not import other adapters; the
// composition root (cmd/syncr-controller) wires them together.
package api

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// maxRequestBytes caps the size of a decoded API request message. A RunJob
// request carries a whole JobSpec (remotes with credentials, filters), which
// the jobs service itself limits to 1 MiB encoded; the cap leaves no room for
// more than a little envelope.
const maxRequestBytes = 1 << 20

// Defaults for the zero fields of Deps.
const (
	defaultRequestTimeout   = 30 * time.Second
	defaultWatchMaxDuration = time.Hour
	defaultWatchKeepalive   = 30 * time.Second
)

// Deps are the collaborators of the API handler.
type Deps struct {
	// BuildInfo is reported by SystemService.GetInfo.
	BuildInfo version.Info
	// Logger receives handler panics and other server-side errors. Defaults
	// to discarding.
	Logger *slog.Logger

	// Auth protects WorkerAdminService and JobService. If nil, those
	// services are not mounted at all (fail closed) and a warning is logged:
	// there is no unauthenticated mode. Milestone 2 passes an AdminTokenAuth
	// (when api.admin_token is configured); Milestone 3 a session
	// authenticator.
	Auth Authenticator
	// Workers and Jobs are the use cases behind the protected services. A
	// nil one leaves its service unmounted.
	Workers WorkerService
	Jobs    JobService

	// BaseContext ends open WatchJob streams when it is done (the
	// composition root cancels it when shutdown starts, before
	// http.Server.Shutdown, which would otherwise wait for the streams).
	// Defaults to context.Background().
	BaseContext context.Context //nolint:containedctx // a lifetime signal, not a request context
	// RequestTimeout bounds each unary call of the protected services
	// (default 30s).
	RequestTimeout time.Duration
	// WatchMaxDuration bounds a WatchJob stream (default 1h).
	WatchMaxDuration time.Duration
	// WatchKeepalive is the idle time after which WatchJob repeats the last
	// job message (default 30s).
	WatchKeepalive time.Duration
}

// NewHandler returns the controller's HTTP handler: the Connect services
// under their /syncr.v1.<Service>/ paths and GET /healthz.
//
// SystemService and /healthz are public. WorkerAdminService and JobService
// are mounted only when Deps.Auth, Deps.Workers and Deps.Jobs are set, and
// then every call must pass Deps.Auth.
func NewHandler(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	opts := []connect.HandlerOption{
		connect.WithRecover(recoverHandler(logger)),
		connect.WithReadMaxBytes(maxRequestBytes),
	}

	mux := http.NewServeMux()
	mux.Handle(syncrv1connect.NewSystemServiceHandler(NewSystemService(deps.BuildInfo), opts...))
	mux.HandleFunc("GET /healthz", healthz)

	switch {
	case deps.Auth == nil:
		logger.Warn("worker and job APIs are disabled: no admin token is configured (set api.admin_token or SYNCR_ADMIN_TOKEN)")
	case deps.Workers == nil || deps.Jobs == nil:
		logger.Warn("worker and job APIs are disabled: the services are not wired")
	default:
		mountProtected(mux, deps, logger, opts)
	}
	return mux
}

// mountProtected adds WorkerAdminService and JobService behind the
// authenticator and the unary timeout.
func mountProtected(mux *http.ServeMux, deps Deps, logger *slog.Logger, opts []connect.HandlerOption) {
	base := deps.BaseContext
	if base == nil {
		base = context.Background()
	}
	timeout := cmp.Or(deps.RequestTimeout, defaultRequestTimeout)
	// The authenticator runs first, so unauthenticated calls cost nothing.
	protected := append(slices.Clone(opts), connect.WithInterceptors(
		&authInterceptor{auth: deps.Auth, log: logger},
		unaryTimeout(timeout),
	))
	errs := errorMapper{log: logger}

	mux.Handle(syncrv1connect.NewWorkerAdminServiceHandler(&workerAdminHandler{workers: deps.Workers, errs: errs}, protected...))
	watchMax := cmp.Or(deps.WatchMaxDuration, defaultWatchMaxDuration)
	jobsPath, jobsHandler := syncrv1connect.NewJobServiceHandler(&jobHandler{
		jobs:      deps.Jobs,
		errs:      errs,
		log:       logger,
		base:      base,
		watchMax:  watchMax,
		keepalive: cmp.Or(deps.WatchKeepalive, defaultWatchKeepalive),
	}, protected...)
	mux.Handle(jobsPath, extendWatchDeadlines(jobsHandler, watchMax+watchDeadlineGrace, logger))
}

// watchDeadlineGrace is added to the watch duration limit for the
// connection deadlines of a WatchJob stream.
const watchDeadlineGrace = 30 * time.Second

// extendWatchDeadlines lifts the connection's read and write deadlines of a
// WatchJob call to limit, so the server-wide http.Server ReadTimeout and
// WriteTimeout (meant for ordinary calls) cannot cut a stream short: a read
// deadline would cancel the stream's context, a write deadline would drop
// its messages. The stream itself ends after the watch duration limit.
func extendWatchDeadlines(next http.Handler, limit time.Duration, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == syncrv1connect.JobServiceWatchJobProcedure {
			rc := http.NewResponseController(w)
			deadline := time.Now().Add(limit)
			if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				logger.WarnContext(r.Context(), "cannot extend the deadlines of a job watch", "error", err.Error())
			}
		}
		next.ServeHTTP(w, r)
	})
}

// unaryTimeout bounds unary calls; streams are bounded by their handlers.
func unaryTimeout(d time.Duration) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, req)
		}
	})
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
