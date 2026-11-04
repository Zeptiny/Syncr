// Package passive is the worker's transport in passive mode: it serves
// WorkerLinkService (Command, Pull) over TLS for the controller, delegating
// to internal/worker/linkcore (core.HandleCommandRequest, core.HandlePull).
//
// # Security
//
//   - TLS 1.2 or later with the worker's certificate and key, loaded from
//     PEM files with the standard library. The controller pins the
//     certificate's SHA-256 fingerprint (Fingerprint returns it), so the
//     certificate is typically self-signed.
//   - Every call must carry link.PassiveAuthorization with the worker's
//     secret. The check runs as a Connect request gate, before the request
//     body is read, and compares SHA-256 digests in constant time. Failures
//     get one fixed Unauthenticated error.
//   - Requests are limited to Options.ReadMaxBytes (raw body and decoded
//     message) and the server has read, write and idle timeouts.
//
// # Shutdown
//
// The composition root, after the agent has shut down (which leaves every
// job's CANCELLED result in the outbox), calls Shutdown with a deadline: the
// server keeps answering Pulls until the controller has acknowledged every
// event (core.WaitDrained) or the deadline passes, then stops gracefully.
//
// Allowed dependencies: the standard library, connect, the generated code,
// internal/link and internal/worker/linkcore.
package passive

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Defaults for Options.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 2 * time.Minute
	DefaultWriteTimeout      = 2 * time.Minute
	DefaultIdleTimeout       = 2 * time.Minute
	// DefaultReadMaxBytes caps a request: a CommandRequest carries up to 64
	// commands with job specs of up to 1 MiB each.
	DefaultReadMaxBytes = 64 << 20
	// MinSecretLen is the shortest accepted worker secret.
	MinSecretLen = 16
	// stopGrace bounds the HTTP shutdown once the drain deadline passed.
	stopGrace = 2 * time.Second
)

// Core is the link core the server delegates to; *linkcore.Core implements
// it.
type Core interface {
	HandleCommandRequest(ctx context.Context, req *syncrv1.CommandRequest) *syncrv1.CommandResponse
	HandlePull(req *syncrv1.PullRequest) *syncrv1.PullResponse
	WaitDrained(ctx context.Context) error
}

// Options configure a Server. Addr, CertFile, KeyFile and Secret are
// required; zero values elsewhere select the defaults.
type Options struct {
	// Addr is the TCP listen address, e.g. ":7443".
	Addr string
	// CertFile and KeyFile are the PEM-encoded certificate (chain, leaf
	// first) and private key.
	CertFile, KeyFile string
	// Secret is the worker secret the controller presents; at least
	// MinSecretLen bytes.
	Secret string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// ReadMaxBytes caps a request.
	ReadMaxBytes int

	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Server serves WorkerLinkService over TLS. Create it with New.
type Server struct {
	core        Core
	addr        string
	secret      [sha256.Size]byte
	fingerprint string
	log         *slog.Logger
	handler     http.Handler
	srv         *http.Server
}

// errBadCredentials is the only error unauthenticated callers see.
var errBadCredentials = errors.New("invalid controller credentials")

// New loads the certificate and key and returns a Server.
func New(core Core, opts Options) (*Server, error) {
	if core == nil {
		return nil, errors.New("passive transport: core is nil")
	}
	if opts.Addr == "" {
		return nil, errors.New("passive transport: listen address is required")
	}
	if len(opts.Secret) < MinSecretLen {
		return nil, fmt.Errorf("passive transport: the worker secret must be at least %d bytes", MinSecretLen)
	}
	for _, d := range []time.Duration{opts.ReadHeaderTimeout, opts.ReadTimeout, opts.WriteTimeout, opts.IdleTimeout} {
		if d < 0 {
			return nil, fmt.Errorf("passive transport: negative timeout %v", d)
		}
	}
	if opts.ReadMaxBytes < 0 {
		return nil, fmt.Errorf("passive transport: negative read limit %d", opts.ReadMaxBytes)
	}
	cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("passive transport: load certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("passive transport: parse certificate: %w", err)
	}
	cert.Leaf = leaf
	sum := sha256.Sum256(cert.Certificate[0])
	s := &Server{
		core:        core,
		addr:        opts.Addr,
		secret:      sha256.Sum256([]byte(opts.Secret)),
		fingerprint: hex.EncodeToString(sum[:]),
		log:         opts.Logger,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if now := time.Now(); now.After(leaf.NotAfter) || now.Before(leaf.NotBefore) {
		// The controller pins the certificate and ignores its validity
		// period, so this is only worth a warning.
		s.log.Warn("the TLS certificate is outside its validity period", "not_before", leaf.NotBefore, "not_after", leaf.NotAfter)
	}

	readMax := cmp.Or(opts.ReadMaxBytes, DefaultReadMaxBytes)
	path, h := syncrv1connect.NewWorkerLinkServiceHandler(&service{core: core},
		connect.WithReadMaxBytes(readMax),
		connect.WithRequestGate(s.authenticate),
		connect.WithRecover(s.recover),
	)
	mux := http.NewServeMux()
	mux.Handle(path, http.MaxBytesHandler(h, int64(readMax)))
	s.handler = mux

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: cmp.Or(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
		ReadTimeout:       cmp.Or(opts.ReadTimeout, DefaultReadTimeout),
		WriteTimeout:      cmp.Or(opts.WriteTimeout, DefaultWriteTimeout),
		IdleTimeout:       cmp.Or(opts.IdleTimeout, DefaultIdleTimeout),
		MaxHeaderBytes:    64 << 10,
		Protocols:         protocols,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	return s, nil
}

// Fingerprint returns the SHA-256 fingerprint of the served certificate's
// DER encoding as 64 lower-case hex digits: what the controller pins.
func (s *Server) Fingerprint() string { return s.fingerprint }

// Handler returns the WorkerLinkService handler without TLS, for tests and
// for serving behind a TLS-terminating component.
func (s *Server) Handler() http.Handler { return s.handler }

// Listen opens the TCP listener on Options.Addr.
func (s *Server) Listen(ctx context.Context) (net.Listener, error) {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("passive transport: listen: %w", err)
	}
	return ln, nil
}

// Serve serves TLS on ln until Shutdown, then returns nil. It closes ln.
func (s *Server) Serve(ln net.Listener) error {
	s.log.Info("serving the worker link", "addr", ln.Addr().String(), "cert_fingerprint", s.fingerprint)
	err := s.srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("passive transport: serve: %w", err)
}

// Shutdown keeps answering Pulls until the controller has acknowledged
// every event or ctx ends, then stops the server gracefully (in-flight
// calls finish; at most a short grace if ctx has already ended). It returns
// an error if events were left undelivered or the server did not stop
// cleanly.
func (s *Server) Shutdown(ctx context.Context) error {
	drainErr := s.core.WaitDrained(ctx)
	if drainErr != nil {
		s.log.WarnContext(ctx, "stopping with undelivered events", "error", drainErr.Error())
	}
	stopCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		stopCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), stopGrace)
		defer cancel()
	}
	err := s.srv.Shutdown(stopCtx)
	if err != nil {
		_ = s.srv.Close()
		err = fmt.Errorf("passive transport: shutdown: %w", err)
	}
	return errors.Join(drainErr, err)
}

// authenticate is the request gate: the controller must present the
// worker's secret.
func (s *Server) authenticate(ctx context.Context, spec connect.Spec, peer connect.Peer, header http.Header) (context.Context, error) {
	secret, err := link.ParsePassiveAuthorization(header.Get("Authorization"))
	got := sha256.Sum256([]byte(secret))
	if err != nil || subtle.ConstantTimeCompare(got[:], s.secret[:]) != 1 {
		s.log.WarnContext(ctx, "rejected link call with invalid credentials", "procedure", spec.Procedure, "peer", peer.Addr)
		return nil, connect.NewError(connect.CodeUnauthenticated, errBadCredentials)
	}
	return ctx, nil
}

func (s *Server) recover(ctx context.Context, spec connect.Spec, _ http.Header, v any) error {
	s.log.ErrorContext(ctx, "link handler panicked", "procedure", spec.Procedure, "panic", fmt.Sprint(v))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// service implements WorkerLinkService.
type service struct {
	core Core
}

var _ syncrv1connect.WorkerLinkServiceHandler = (*service)(nil)

func (h *service) Command(ctx context.Context, req *connect.Request[syncrv1.CommandRequest]) (*connect.Response[syncrv1.CommandResponse], error) {
	return connect.NewResponse(h.core.HandleCommandRequest(ctx, req.Msg)), nil
}

func (h *service) Pull(_ context.Context, req *connect.Request[syncrv1.PullRequest]) (*connect.Response[syncrv1.PullResponse], error) {
	return connect.NewResponse(h.core.HandlePull(req.Msg)), nil
}
