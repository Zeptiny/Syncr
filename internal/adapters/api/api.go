// Package api is the controller's inbound HTTP adapter: it serves the
// Connect API (proto/syncr/v1/*.proto, except the worker link), the
// embedded web UI and the health endpoint, translating between wire
// messages and application services.
//
// # Access control
//
// Every API service runs behind one interceptor (policy.go): it looks up the
// procedure in the policy table (public, user, user with a session, admin;
// a procedure without an entry is denied), authenticates non-public calls
// with Deps.Auth, which puts the Caller into the context, and checks the
// caller's role and authentication method. Handlers read the actor with
// ActorFrom and pass it to the use cases, which scope objects by it (a
// user's own remotes and jobs). The whole handler is wrapped in
// http.CrossOriginProtection, so browsers cannot send cross-origin
// state-changing requests.
//
// Without an authenticator only the public SystemService, /healthz and the
// web UI are served (fail closed) and a warning is logged.
//
// Streaming calls are authenticated again every Deps.StreamReauthInterval
// while they are open, ending with Unauthenticated or PermissionDenied once
// the caller's credentials or access are gone (streamauth.go).
//
// WatchJob streams extend their connection's read and write deadlines (to
// Deps.WatchMaxDuration plus a grace period), so the composition root may set
// http.Server ReadTimeout and WriteTimeout for ordinary calls without cutting
// streams short.
//
// Allowed dependencies: the standard library, connect, the generated
// protobuf code, internal/domain, internal/app/... services,
// internal/backend, internal/descriptor, internal/notify, internal/jobkind
// and internal/platform packages. It must
// not import other adapters; the composition root (cmd/syncr-controller)
// wires them together.
package api

import (
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// maxRequestBytes caps the size of a decoded API request message. The
// largest requests (a job definition with filters, a remote with a PEM key)
// are far below it.
const maxRequestBytes = 1 << 20

// Defaults for the zero fields of Deps.
const (
	defaultRequestTimeout   = 30 * time.Second
	defaultWatchMaxDuration = time.Hour
	defaultWatchKeepalive   = 30 * time.Second
)

// Deps are the collaborators of the API handler. A nil service leaves the
// API services built on it unmounted (their calls fail with Unimplemented).
type Deps struct {
	// BuildInfo is reported by SystemService.GetInfo.
	BuildInfo version.Info
	// Logger receives handler panics and other server-side errors. Defaults
	// to discarding.
	Logger *slog.Logger

	// Auth authenticates the calls of non-public procedures. If nil, no
	// service but SystemService is mounted (fail closed) and a warning is
	// logged: there is no unauthenticated mode.
	Auth Authenticator

	// Identity serves SetupService and AuthService (*users.Service).
	Identity IdentityService
	// Users serves UserService (*users.Service).
	Users UserAdminService
	// Workers serves WorkerAdminService (*workers.Service).
	Workers WorkerService
	// Jobs serves JobService (*jobs.Service), with Kinds checking the job
	// definitions of RunJob (without Kinds, RunJob is Unimplemented) and
	// TimeLimits computing Job.time_limit of queued jobs (*dispatch.Specs;
	// without it the field is unset for them; a dispatched job shows the
	// limit stored when its worker accepted it).
	Jobs       JobService
	Kinds      *jobkind.Registry
	TimeLimits TimeLimits
	// Remotes serves RemoteService (*remotes.Service).
	Remotes RemoteService
	// Backends serves BackendService: the backend type descriptors.
	Backends *backend.Registry

	// Schedules serves ScheduleService (*schedules.Service), with Kinds
	// checking the definitions, Remotes naming their remotes and Contacts
	// naming their contacts. With Jobs (implementing DashboardJobs) and
	// Workers it also serves DashboardService.
	Schedules ScheduleService
	// Contacts serves ContactService (*contacts.Service), with Deliveries
	// (*notifications.Service) serving its delivery log.
	Contacts   ContactService
	Deliveries DeliveryService
	// Channels serves ChannelService (the notification channel
	// descriptors) and validates contacts' channel types.
	Channels *notify.Registry

	// Restic serves ResticService (*restic.Service) and names the
	// repositories of jobs and schedules.
	Restic ResticService
	// Probes serves ProbeService (*probe.Service of the controller).
	Probes ProbeService

	// ProbeTimeout is the longest a worker probe may take (the larger of
	// the controller's probe timeouts; default 60s). The calls that wait
	// for one (ProbeService, and the ResticService calls that create,
	// verify, list and browse through a worker) get this long plus a
	// margin instead of RequestTimeout, and their connection deadlines are
	// extended like a WatchJob's.
	ProbeTimeout time.Duration

	// UI is the web UI's files (index.html at the root); nil serves no UI.
	UI fs.FS
	// Cookies configures the session cookie.
	Cookies CookieConfig
	// ClientIP derives the client address of Login and CompleteSetup
	// calls, for their rate limits and the session record
	// (clientip.Resolver.ClientIP). Nil uses the TCP peer's address.
	ClientIP ClientIPFunc
	// Install configures the worker install and upgrade commands; an empty
	// Install.Version is BuildInfo.Version.
	Install InstallConfig

	// BaseContext ends open WatchJob streams when it is done (the
	// composition root cancels it when shutdown starts, before
	// http.Server.Shutdown, which would otherwise wait for the streams).
	// Defaults to context.Background().
	BaseContext context.Context //nolint:containedctx // a lifetime signal, not a request context
	// RequestTimeout bounds each unary call (default 30s).
	RequestTimeout time.Duration
	// WatchMaxDuration bounds a WatchJob stream (default 1h).
	WatchMaxDuration time.Duration
	// WatchKeepalive is the idle time after which WatchJob repeats the last
	// job message (default 30s).
	WatchKeepalive time.Duration
	// StreamReauthInterval is how often an open stream (WatchJob) is
	// authenticated again, so revoked credentials, disabled users and lost
	// access end it (default 30s).
	StreamReauthInterval time.Duration
}

// NewHandler returns the controller's HTTP handler: the Connect services
// under their /syncr.v1.<Service>/ paths, GET /healthz and, with Deps.UI,
// the web UI for every other path. See the package documentation for access
// control.
func NewHandler(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// The guard runs first, so unauthenticated calls cost nothing.
	guard := newGuard(deps.Auth, logger)
	guard.reauth = cmp.Or(deps.StreamReauthInterval, defaultStreamReauth)
	opts := []connect.HandlerOption{
		connect.WithRecover(recoverHandler(logger)),
		connect.WithReadMaxBytes(maxRequestBytes),
		connect.WithInterceptors(
			guard,
			unaryTimeout(cmp.Or(deps.RequestTimeout, defaultRequestTimeout), deps.ProbeTimeout),
		),
	}

	mux := http.NewServeMux()
	mux.Handle(syncrv1connect.NewSystemServiceHandler(NewSystemService(deps.BuildInfo), opts...))
	mux.HandleFunc("GET /healthz", healthz)
	if deps.Auth == nil {
		logger.Warn("protected APIs are disabled: no authenticator is configured")
	} else {
		mountProtected(mux, deps, logger, opts)
	}
	if deps.UI != nil {
		mux.Handle("/", newSPAHandler(deps.UI, logger))
	}
	return http.NewCrossOriginProtection().Handler(withTLSState(mux))
}

// mountProtected adds the services that need an authenticated caller for
// some or all of their procedures, each when its use cases are wired.
func mountProtected(mux *http.ServeMux, deps Deps, logger *slog.Logger, opts []connect.HandlerOption) {
	errs := errorMapper{log: logger}
	deps.Install.Version = cmp.Or(deps.Install.Version, deps.BuildInfo.Version)
	disabled := func(service string) {
		logger.Info("api service disabled: its use cases are not wired", "service", service)
	}

	if deps.Identity != nil {
		mux.Handle(syncrv1connect.NewSetupServiceHandler(&setupHandler{identity: deps.Identity, cookies: deps.Cookies, clientIP: deps.ClientIP, errs: errs}, opts...))
		mux.Handle(syncrv1connect.NewAuthServiceHandler(&authHandler{identity: deps.Identity, cookies: deps.Cookies, clientIP: deps.ClientIP, errs: errs}, opts...))
	} else {
		disabled(syncrv1connect.SetupServiceName)
		disabled(syncrv1connect.AuthServiceName)
	}
	if deps.Users != nil {
		mux.Handle(syncrv1connect.NewUserServiceHandler(&userHandler{users: deps.Users, errs: errs}, opts...))
	} else {
		disabled(syncrv1connect.UserServiceName)
	}
	if deps.Workers != nil {
		mux.Handle(syncrv1connect.NewWorkerAdminServiceHandler(&workerAdminHandler{workers: deps.Workers, install: deps.Install, errs: errs}, opts...))
	} else {
		disabled(syncrv1connect.WorkerAdminServiceName)
	}
	if deps.Jobs != nil {
		base := deps.BaseContext
		if base == nil {
			base = context.Background()
		}
		watchMax := cmp.Or(deps.WatchMaxDuration, defaultWatchMaxDuration)
		jobsPath, jobsHandler := syncrv1connect.NewJobServiceHandler(&jobHandler{
			jobs:      deps.Jobs,
			kinds:     deps.Kinds,
			limits:    deps.TimeLimits,
			remotes:   deps.Remotes,
			schedules: deps.Schedules,
			repos:     repositoryNames(deps),
			sources:   remoteSourceChecker(deps),
			workers:   workerNames(deps),
			errs:      errs,
			log:       logger,
			base:      base,
			watchMax:  watchMax,
			keepalive: cmp.Or(deps.WatchKeepalive, defaultWatchKeepalive),
		}, opts...)
		mux.Handle(jobsPath, extendWatchDeadlines(jobsHandler, watchMax+watchDeadlineGrace, logger))
	} else {
		disabled(syncrv1connect.JobServiceName)
	}
	if deps.Backends != nil {
		mux.Handle(syncrv1connect.NewBackendServiceHandler(&backendHandler{backends: deps.Backends}, opts...))
	} else {
		disabled(syncrv1connect.BackendServiceName)
	}
	if deps.Remotes != nil {
		mux.Handle(syncrv1connect.NewRemoteServiceHandler(&remoteHandler{remotes: deps.Remotes, backends: deps.Backends, errs: errs}, opts...))
	} else {
		disabled(syncrv1connect.RemoteServiceName)
	}
	mountMilestone4(mux, deps, errs, disabled, opts)
	mountMilestone5(mux, deps, errs, disabled, opts)
}

// mountMilestone5 adds the restic repository and probe services.
func mountMilestone5(mux *http.ServeMux, deps Deps, errs errorMapper, disabled func(string), opts []connect.HandlerOption) {
	if deps.Restic != nil {
		path, handler := syncrv1connect.NewResticServiceHandler(&resticHandler{
			restic: deps.Restic, remotes: remoteNames(deps), workers: workerNames(deps), errs: errs,
		}, opts...)
		mux.Handle(path, extendProbeDeadlines(handler, deps.ProbeTimeout, errs.log))
	} else {
		disabled(syncrv1connect.ResticServiceName)
	}
	if deps.Probes != nil {
		path, handler := syncrv1connect.NewProbeServiceHandler(&probeHandler{probes: deps.Probes, errs: errs}, opts...)
		mux.Handle(path, extendProbeDeadlines(handler, deps.ProbeTimeout, errs.log))
	} else {
		disabled(syncrv1connect.ProbeServiceName)
	}
}

// mountMilestone4 adds the schedule, notification and dashboard services.
func mountMilestone4(mux *http.ServeMux, deps Deps, errs errorMapper, disabled func(string), opts []connect.HandlerOption) {
	if deps.Schedules != nil {
		mux.Handle(syncrv1connect.NewScheduleServiceHandler(&scheduleHandler{
			schedules: deps.Schedules, kinds: deps.Kinds, remotes: deps.Remotes, contacts: deps.Contacts,
			repos: repositoryNames(deps), sources: remoteSourceChecker(deps), workers: workerNames(deps), errs: errs,
		}, opts...))
	} else {
		disabled(syncrv1connect.ScheduleServiceName)
	}
	if deps.Contacts != nil {
		mux.Handle(syncrv1connect.NewContactServiceHandler(&contactHandler{
			contacts: deps.Contacts, deliveries: deps.Deliveries, channels: deps.Channels, errs: errs,
		}, opts...))
	} else {
		disabled(syncrv1connect.ContactServiceName)
	}
	if deps.Channels != nil {
		mux.Handle(syncrv1connect.NewChannelServiceHandler(&channelHandler{channels: deps.Channels}, opts...))
	} else {
		disabled(syncrv1connect.ChannelServiceName)
	}
	activity, ok := deps.Jobs.(DashboardJobs)
	if ok && deps.Workers != nil && deps.Schedules != nil {
		mux.Handle(syncrv1connect.NewDashboardServiceHandler(&dashboardHandler{
			jobs: deps.Jobs, activity: activity, workers: deps.Workers, schedules: deps.Schedules, remotes: deps.Remotes,
			kinds: deps.Kinds, repos: repositoryNames(deps), version: deps.Install.Version, errs: errs,
		}, opts...))
	} else {
		disabled(syncrv1connect.DashboardServiceName)
	}
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

// unaryTimeout bounds unary calls with d, and the calls that wait for a
// worker's probe (probeProcedure) with the probe's time limit if that is
// longer (see probeCallTimeout); streams are bounded by their handlers.
func unaryTimeout(d, probe time.Duration) connect.Interceptor {
	slow := max(d, probeCallTimeout(probe))
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			d := d
			if probeProcedure(req.Spec().Procedure) {
				d = slow
			}
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
