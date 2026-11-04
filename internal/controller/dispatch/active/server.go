package active

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Defaults for Options.
const (
	// DefaultReadMaxBytes caps a link request (Report, Poll).
	DefaultReadMaxBytes = 16 << 20
	// DefaultEnrollReadMaxBytes caps an Enroll request.
	DefaultEnrollReadMaxBytes = 64 << 10
	// DefaultPollGrace is added to the dispatcher's MaxWait for the
	// connection deadlines of a Poll.
	DefaultPollGrace = 15 * time.Second
)

// Dispatcher is the part of *dispatch.Service the link handlers use.
type Dispatcher interface {
	Report(ctx context.Context, workerID domain.WorkerID, req *syncrv1.ReportRequest) (*syncrv1.ReportResponse, error)
	Poll(ctx context.Context, workerID domain.WorkerID, req *syncrv1.PollRequest) (*syncrv1.PollResponse, error)
	// MaxWait is the longest a Poll may wait for commands.
	MaxWait() time.Duration
}

// Workers is the part of *workers.Service the handlers use.
type Workers interface {
	Authenticate(ctx context.Context, id domain.WorkerID, secret string) (workers.Worker, error)
	Enroll(ctx context.Context, p workers.EnrollParams) (workers.Enrollment, error)
}

// Limiter decides whether a client may make another Enroll call. It must be
// safe for concurrent use. RateLimiter implements it.
type Limiter interface {
	Allow(key string) bool
}

// ClientKeyFunc derives the rate-limit key of an Enroll call.
type ClientKeyFunc func(peer connect.Peer, header http.Header) string

// Options configure a Server. Dispatcher, Workers and Lifetime are
// required; zero values elsewhere select the defaults.
type Options struct {
	Dispatcher Dispatcher
	Workers    Workers
	// Lifetime ends every pending long poll when it is done. Pass the
	// context whose end starts the HTTP server's graceful shutdown.
	Lifetime context.Context

	// ReadMaxBytes caps a Report or Poll request (raw body and decoded
	// message). Zero means DefaultReadMaxBytes.
	ReadMaxBytes int
	// EnrollReadMaxBytes caps an Enroll request. Zero means
	// DefaultEnrollReadMaxBytes.
	EnrollReadMaxBytes int
	// PollGrace is added to the dispatcher's MaxWait for the read and write
	// deadlines of a Poll's connection. Zero means DefaultPollGrace.
	PollGrace time.Duration

	// EnrollLimiter limits Enroll calls per client key. Nil means
	// NewRateLimiter(DefaultRateLimit(), nil).
	EnrollLimiter Limiter
	// ClientKey derives an Enroll call's rate-limit key. Nil means
	// PeerIP: the IP address of the TCP peer. Behind a reverse proxy, pass a
	// function that reads the proxy's forwarding header, and make sure the
	// proxy overwrites it.
	ClientKey ClientKeyFunc

	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Server serves ControllerLinkService and EnrollmentService. Create it with
// New; it is an http.Handler for both services' paths.
type Server struct {
	disp      Dispatcher
	workers   Workers
	lifetime  context.Context
	limiter   Limiter
	clientKey ClientKeyFunc
	pollGrace time.Duration
	log       *slog.Logger

	mux *http.ServeMux
}

// New returns a Server.
func New(opts Options) (*Server, error) {
	if opts.Dispatcher == nil || opts.Workers == nil || opts.Lifetime == nil {
		return nil, errors.New("active link server: missing dependency")
	}
	if opts.ReadMaxBytes < 0 || opts.EnrollReadMaxBytes < 0 || opts.PollGrace < 0 {
		return nil, fmt.Errorf("active link server: invalid limits %d, %d, %v", opts.ReadMaxBytes, opts.EnrollReadMaxBytes, opts.PollGrace)
	}
	s := &Server{
		disp:      opts.Dispatcher,
		workers:   opts.Workers,
		lifetime:  opts.Lifetime,
		limiter:   opts.EnrollLimiter,
		clientKey: opts.ClientKey,
		pollGrace: cmp.Or(opts.PollGrace, DefaultPollGrace),
		log:       opts.Logger,
		mux:       http.NewServeMux(),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.limiter == nil {
		s.limiter = NewRateLimiter(DefaultRateLimit(), nil)
	}
	if s.clientKey == nil {
		s.clientKey = PeerIP
	}
	readMax := cmp.Or(opts.ReadMaxBytes, DefaultReadMaxBytes)
	enrollMax := cmp.Or(opts.EnrollReadMaxBytes, DefaultEnrollReadMaxBytes)

	path, h := syncrv1connect.NewControllerLinkServiceHandler(&linkService{s: s},
		connect.WithRecover(s.recover),
		connect.WithReadMaxBytes(readMax),
		connect.WithRequestGate(s.authenticate),
	)
	s.mux.Handle(path, http.MaxBytesHandler(s.extendPollDeadlines(h), int64(readMax)))

	path, h = syncrv1connect.NewEnrollmentServiceHandler(&enrollService{s: s},
		connect.WithRecover(s.recover),
		connect.WithReadMaxBytes(enrollMax),
		connect.WithRequestGate(s.rateLimit),
	)
	s.mux.Handle(path, http.MaxBytesHandler(h, int64(enrollMax)))
	return s, nil
}

// ServeHTTP serves the link services' paths and answers 404 for anything
// else.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Mount registers s on mux under the paths of ControllerLinkService and
// EnrollmentService.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.Handle("/"+syncrv1connect.ControllerLinkServiceName+"/", s)
	mux.Handle("/"+syncrv1connect.EnrollmentServiceName+"/", s)
}

// workerIDKey carries the authenticated worker ID in a request context.
type workerIDKey struct{}

// errBadCredentials is the only error unauthenticated callers ever see.
var errBadCredentials = errors.New("invalid worker credentials")

// authenticate is the request gate of ControllerLinkService.
func (s *Server) authenticate(ctx context.Context, spec connect.Spec, peer connect.Peer, header http.Header) (context.Context, error) {
	rawID, secret, err := link.ParseActiveAuthorization(header.Get("Authorization"))
	var id domain.WorkerID
	if err == nil {
		id, err = domain.ParseWorkerID(rawID)
	}
	if err == nil {
		_, err = s.workers.Authenticate(ctx, id, secret)
	}
	switch {
	case err == nil:
		return context.WithValue(ctx, workerIDKey{}, id), nil
	case errors.Is(err, link.ErrBadAuthorization), errors.Is(err, domain.ErrInvalidID), errors.Is(err, domain.ErrUnauthenticated):
		s.log.InfoContext(ctx, "rejected link call with invalid credentials", "procedure", spec.Procedure, "peer", peer.Addr)
		return nil, connect.NewError(connect.CodeUnauthenticated, errBadCredentials)
	default:
		return nil, s.toConnect(ctx, spec.Procedure, err)
	}
}

// workerID returns the worker authenticated by the gate.
func workerID(ctx context.Context) (domain.WorkerID, error) {
	id, ok := ctx.Value(workerIDKey{}).(domain.WorkerID)
	if !ok || id.IsZero() {
		// Unreachable: the gate runs before every handler.
		return domain.WorkerID{}, connect.NewError(connect.CodeUnauthenticated, errBadCredentials)
	}
	return id, nil
}

// extendPollDeadlines moves the connection's read and write deadlines of a
// Poll past the longest wait, so server-wide timeouts cannot cut it. They do
// not leak to the next request on the connection: net/http clears the write
// deadline after each response and sets the read deadline per request.
func (s *Server) extendPollDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != syncrv1connect.ControllerLinkServicePollProcedure {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(s.disp.MaxWait() + s.pollGrace)
		if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			s.log.WarnContext(r.Context(), "cannot extend the deadlines of a long poll", "error", err.Error())
		}
		next.ServeHTTP(w, r)
	})
}

// recover turns a handler panic into an Internal error.
func (s *Server) recover(ctx context.Context, spec connect.Spec, _ http.Header, v any) error {
	s.log.ErrorContext(ctx, "link handler panicked", "procedure", spec.Procedure, "panic", fmt.Sprint(v))
	return connect.NewError(connect.CodeInternal, errInternal)
}

// PeerIP is the default ClientKeyFunc: the IP address of the TCP peer.
func PeerIP(peer connect.Peer, _ http.Header) string {
	if host, _, err := net.SplitHostPort(peer.Addr); err == nil {
		return host
	}
	return peer.Addr
}
