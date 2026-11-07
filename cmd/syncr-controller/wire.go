package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/internal/adapters/api"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/postgres"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/active"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clientip"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/httpserver"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// store is everything the use cases persist: the Postgres store in
// production, the in-memory store with dev.memory_store.
type store interface {
	workers.Repository
	workers.TokenRepository
	jobs.Repository
	jobs.SampleRepository
	users.Repository
	users.SessionRepository
	users.APITokenRepository
	remotes.Repository
}

// openStore opens and migrates the configured store. close releases it.
func openStore(ctx context.Context, cfg controllerconfig.Config, log *slog.Logger) (st store, closeStore func(), err error) {
	if cfg.Dev.MemoryStore {
		log.Warn("DEVELOPMENT MODE: dev.memory_store is on; workers, tokens and jobs live in memory and are LOST when the controller stops. " +
			"Never use this in production: set database.url (SYNCR_DATABASE_URL) instead")
		return memstore.New(), func() {}, nil
	}
	pcfg := postgres.DefaultConfig()
	pcfg.URL = cfg.Database.URL
	pcfg.MaxConns = cfg.Database.MaxConns
	pcfg.ConnectTimeout = cfg.Database.ConnectTimeout
	log.Info("connecting to the database")
	pg, err := postgres.Open(ctx, pcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("open the database: %w", err)
	}
	if err := pg.Migrate(ctx); err != nil {
		pg.Close()
		return nil, nil, fmt.Errorf("migrate the database: %w", err)
	}
	log.Info("database ready (schema migrated)")
	return pg, pg.Close, nil
}

// newKeyring builds the secrets cipher from secrets.key, secrets.key_id and
// secrets.previous_keys.
func newKeyring(cfg controllerconfig.Secrets) (*secrets.Keyring, error) {
	active, err := secrets.ParseKey(cfg.KeyID, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("secrets.key: %w", err)
	}
	previous := make([]secrets.Key, 0, len(cfg.PreviousKeys))
	for i, spec := range cfg.PreviousKeys {
		k, err := secrets.ParseKeySpec(spec)
		if err != nil {
			return nil, fmt.Errorf("secrets.previous_keys[%d]: %w", i, err)
		}
		previous = append(previous, k)
	}
	kr, err := secrets.NewKeyring(active, previous...)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return kr, nil
}

// services are the controller's use cases and link machinery over one store.
type services struct {
	jobs    *jobs.Service
	workers *workers.Service
	disp    *dispatch.Service
	// adminWorkers is the workers service as the API and the enrollment
	// handler see it: it wakes the passive supervisor after changes.
	adminWorkers *wakingWorkers
	// wake triggers a resync of the passive supervisor.
	wake chan struct{}
}

// newServices wires the use cases. One Hub is shared by the job service (as
// its Waker) and the dispatcher, so queued jobs and cancel requests wake the
// long polls and passive loops of their worker.
func newServices(st store, cipher *secrets.Keyring, log *slog.Logger) (*services, error) {
	hub := dispatch.NewHub()
	clk := clock.System{}
	ids := idgen.UUIDv7{}
	js, err := jobs.New(jobs.Deps{
		Jobs: st, Samples: st, Clock: clk, IDs: ids,
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: hub,
		Logger: log.With("component", "jobs"),
	}, jobs.Config{})
	if err != nil {
		return nil, fmt.Errorf("jobs service: %w", err)
	}
	ws, err := workers.New(workers.Deps{
		Workers: st, Tokens: st, Cipher: cipher, Clock: clk, IDs: ids,
		Jobs: js, Random: rand.Reader,
		Logger: log.With("component", "workers"),
	}, workers.Config{})
	if err != nil {
		return nil, fmt.Errorf("workers service: %w", err)
	}
	// m3: pending integration: Specs (remotes.Resolve + job kinds) is wired
	// by the integration pass; until then queued jobs fail with invalid_spec.
	disp, err := dispatch.New(dispatch.Deps{
		Jobs: js, Workers: ws, Clock: clk, Hub: hub,
		Logger: log.With("component", "dispatch"),
	}, dispatch.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	wake := make(chan struct{}, 1)
	return &services{
		jobs: js, workers: ws, disp: disp, wake: wake,
		adminWorkers: &wakingWorkers{Service: ws, wake: wake},
	}, nil
}

// newHandler mounts the API (api.NewHandler) and the worker link
// (ControllerLinkService, EnrollmentService) on one mux. lifetime ends the
// API's watch streams and the link's long polls.
func newHandler(lifetime context.Context, cfg controllerconfig.Config, svc *services, info version.Info, log *slog.Logger) (http.Handler, error) {
	var auth api.Authenticator
	if tok := cfg.API.AdminToken; tok.Reveal() != "" {
		a, err := api.NewAdminTokenAuth(tok)
		if err != nil {
			return nil, fmt.Errorf("api.admin_token: %w", err)
		}
		auth = a
	}
	// m3: pending integration: Identity, Users, Remotes, Backends, Kinds, UI,
	// Cookies and Install are wired (and SessionAuth replaces the admin
	// token) by the integration pass. Until then those services stay
	// unmounted, and RunJob is Unimplemented (no Kinds).
	apiHandler := api.NewHandler(api.Deps{
		BuildInfo:        info,
		Logger:           log.With("component", "api"),
		Auth:             auth,
		Workers:          svc.adminWorkers,
		Jobs:             svc.jobs,
		BaseContext:      lifetime,
		RequestTimeout:   cfg.API.RequestTimeout,
		WatchMaxDuration: cfg.API.WatchMaxDuration,
		WatchKeepalive:   cfg.API.WatchKeepalive,
	})

	clientKey, err := enrollClientKey(cfg.HTTP)
	if err != nil {
		return nil, err
	}
	link, err := active.New(active.Options{
		Dispatcher: svc.disp,
		Workers:    svc.adminWorkers,
		Lifetime:   lifetime,
		ClientKey:  clientKey,
		Logger:     log.With("component", "link"),
	})
	if err != nil {
		return nil, fmt.Errorf("worker link: %w", err)
	}

	mux := http.NewServeMux()
	link.Mount(mux)
	mux.Handle("/", apiHandler)
	return mux, nil
}

// enrollClientKey returns the rate-limit key function of Enroll calls: the
// TCP peer's address, or behind trusted proxies the address in
// http.client_ip_header.
func enrollClientKey(cfg controllerconfig.HTTP) (active.ClientKeyFunc, error) {
	if cfg.ClientIPHeader == "" {
		return nil, nil // active.PeerIP
	}
	trusted, err := controllerconfig.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	r := clientip.New(cfg.ClientIPHeader, trusted)
	return func(peer connect.Peer, header http.Header) string { return r.ClientIP(peer.Addr, header) }, nil
}

// newServer returns the HTTP server with the timeouts the worker link needs
// (see internal/controller/dispatch/active): the long polls and WatchJob
// streams extend their own connection deadlines, so ReadTimeout and
// WriteTimeout only bound ordinary calls.
func newServer(cfg controllerconfig.HTTP, h http.Handler, log *slog.Logger) *http.Server {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)            // over TLS (http.tls_cert_file)
	protocols.SetUnencryptedHTTP2(true) // gRPC clients without TLS (h2c)
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		Protocols:         protocols,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// serve runs the controller until ctx ends, then shuts it down in order:
//
//  1. the server's lifetime context ends: it ends the API's WatchJob streams
//     (api BaseContext) and makes pending long polls answer at once
//     (active Lifetime), so step 2 does not wait for them;
//  2. the HTTP server shuts down gracefully (http.shutdown_timeout);
//  3. the passive supervisor and the dispatch-timeout sweeper stop;
//  4. the store is closed.
//
// If ready is non-nil it receives the listen address once the server
// accepts connections.
func serve(ctx context.Context, cfg controllerconfig.Config, info version.Info, log *slog.Logger, ready chan<- net.Addr) (err error) {
	cipher, err := newKeyring(cfg.Secrets)
	if err != nil {
		return err
	}
	var certs *certReloader
	if cfg.HTTP.TLS() {
		if certs, err = newCertReloader(cfg.HTTP.TLSCertFile, cfg.HTTP.TLSKeyFile, log); err != nil {
			return err
		}
	}

	st, closeStore, err := openStore(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		closeStore()
		log.Info("shutdown: store closed")
	}()

	svc, err := newServices(st, cipher, log)
	if err != nil {
		return err
	}
	// lifetime is the server's lifetime: its end (right after ctx's, once
	// the step is logged) is what starts the graceful shutdown.
	lifetime, endLifetime := context.WithCancel(context.WithoutCancel(ctx))
	defer endLifetime()
	handler, err := newHandler(lifetime, cfg, svc, info, log)
	if err != nil {
		return err
	}
	sup, err := passive.NewSupervisor(passive.SupervisorOptions{
		Workers: svc.workers,
		NewLoop: passive.DispatchLoops(svc.disp),
		Wake:    svc.wake,
		Logger:  log.With("component", "passive"),
	})
	if err != nil {
		return fmt.Errorf("passive supervisor: %w", err)
	}

	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Background loops outlive the HTTP server: they stop in step 3.
	bgCtx, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackground()
	var background sync.WaitGroup
	background.Go(func() { _ = svc.disp.Run(bgCtx) })
	background.Go(func() { _ = sup.Run(bgCtx) })
	stopLoops := func() {
		stopBackground()
		background.Wait()
		log.Info("shutdown: passive supervisor and dispatch sweeper stopped")
	}

	srv := newServer(cfg.HTTP, handler, log)
	scheme := "http"
	if certs != nil {
		scheme = "https"
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.GetCertificate}
	}
	log.Info("controller starting",
		"version", info.Version, "commit", info.Commit, "addr", ln.Addr().String(), "scheme", scheme,
		"store", storeKind(cfg), "admin_api", cfg.API.AdminToken.Reveal() != "")
	served := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Info("shutdown: started; ending watch streams and long polls, draining HTTP requests", "timeout", cfg.HTTP.ShutdownTimeout.String())
			endLifetime()
		case <-served:
		}
	}()
	if ready != nil {
		ready <- ln.Addr()
	}
	if certs != nil {
		err = httpserver.ServeTLS(lifetime, srv, ln, "", "", cfg.HTTP.ShutdownTimeout)
	} else {
		err = httpserver.Serve(lifetime, srv, ln, cfg.HTTP.ShutdownTimeout)
	}
	close(served)
	if err != nil {
		log.Error("http server stopped with an error", "error", err.Error())
	} else {
		log.Info("shutdown: http server stopped")
	}
	stopLoops()
	return err
}

func storeKind(cfg controllerconfig.Config) string {
	if cfg.Dev.MemoryStore {
		return "memory (dev)"
	}
	return "postgres"
}

// wakingWorkers is the workers service as the API and the enrollment
// handler use it. Changes that alter the set of passive workers or their
// targets (registration, passive enrollment, rename, removal) wake the
// passive supervisor, so it starts or stops the worker's loop at once instead
// of at its next periodic resync.
type wakingWorkers struct {
	*workers.Service
	wake chan<- struct{}
}

// Compile-time checks: the decorator serves both consumers.
var (
	_ api.WorkerService = (*wakingWorkers)(nil)
	_ active.Workers    = (*wakingWorkers)(nil)
)

// notify asks for a resync without ever blocking (a pending wake-up covers
// this change too).
func (w *wakingWorkers) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// RegisterPassive registers a passive worker and wakes the supervisor.
func (w *wakingWorkers) RegisterPassive(ctx context.Context, p workers.PassiveParams) (workers.Worker, error) {
	wk, err := w.Service.RegisterPassive(ctx, p)
	if err == nil {
		w.notify()
	}
	return wk, err
}

// Enroll enrolls a worker; a passive one wakes the supervisor.
func (w *wakingWorkers) Enroll(ctx context.Context, p workers.EnrollParams) (workers.Enrollment, error) {
	e, err := w.Service.Enroll(ctx, p)
	if err == nil && e.Worker.Mode == domain.WorkerPassive {
		w.notify()
	}
	return e, err
}

// Rename renames a worker and wakes the supervisor.
func (w *wakingWorkers) Rename(ctx context.Context, id domain.WorkerID, name string) (workers.Worker, error) {
	wk, err := w.Service.Rename(ctx, id, name)
	if err == nil {
		w.notify()
	}
	return wk, err
}

// Remove removes a worker and wakes the supervisor. A failed removal may
// still have changed something, so it wakes it too.
func (w *wakingWorkers) Remove(ctx context.Context, id domain.WorkerID) error {
	err := w.Service.Remove(ctx, id)
	w.notify()
	return err
}

// certReloader serves the certificate in http.tls_cert_file and reloads it
// when the files change (a renewed certificate needs no restart).
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

// certCheckInterval is how often the files are checked for changes.
const certCheckInterval = time.Minute

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, log: log}
	if err := r.load(time.Now()); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) load(now time.Time) error {
	mt, err := latestModTime(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("http.tls_cert_file/http.tls_key_file: %w", err)
	}
	r.checked = now
	if r.cert != nil && mt.Equal(r.modTime) {
		return nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("http.tls_cert_file/http.tls_key_file: %w", err)
	}
	if r.cert != nil {
		r.log.Info("tls certificate reloaded", "cert_file", r.certFile)
	}
	r.cert, r.modTime = &c, mt
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. If a reload fails,
// the previous certificate stays in use and the error is logged.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.checked) >= certCheckInterval {
		if err := r.load(now); err != nil {
			r.log.Error("tls certificate reload failed; serving the previous one", "error", err.Error())
		}
	}
	if r.cert == nil {
		return nil, errors.New("no tls certificate loaded")
	}
	return r.cert, nil
}

// latestModTime returns the newer modification time of two files.
func latestModTime(a, b string) (time.Time, error) {
	var latest time.Time
	for _, f := range []string{a, b} {
		fi, err := os.Stat(f)
		if err != nil {
			return time.Time{}, err
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}
