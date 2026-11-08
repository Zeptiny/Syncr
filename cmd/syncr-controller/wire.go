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
	"github.com/Zeptiny/Syncr/internal/adapters/password"
	"github.com/Zeptiny/Syncr/internal/adapters/postgres"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/active"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/devseed"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/platform/clientip"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/httpserver"
	"github.com/Zeptiny/Syncr/internal/platform/ratelimit"
	"github.com/Zeptiny/Syncr/internal/platform/version"
	"github.com/Zeptiny/Syncr/web"
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
		log.Warn("DEVELOPMENT MODE: dev.memory_store is on; users, remotes, workers and jobs live in memory and are LOST when the controller stops. " +
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
	users    *users.Service
	remotes  *remotes.Service
	backends *backend.Registry
	kinds    *jobkind.Registry
	jobs     *jobs.Service
	workers  *workers.Service
	disp     *dispatch.Service
	// adminWorkers is the workers service as the API and the enrollment
	// handler see it: it wakes the passive supervisor after changes.
	adminWorkers *wakingWorkers
	// wake triggers a resync of the passive supervisor.
	wake chan struct{}
}

// loginLimits are the rate limits of credential checks: per client IP
// burst 10, then 1 every 30 s; per username burst 5, then 1 a minute;
// setup attempts burst 5, then 1 a minute, globally.
func loginLimits() (clientIP, username, setup ratelimit.Config) {
	return ratelimit.Config{Burst: 10, Every: 30 * time.Second, MaxKeys: 10000},
		ratelimit.Config{Burst: 5, Every: time.Minute, MaxKeys: 10000},
		ratelimit.Config{Burst: 5, Every: time.Minute, MaxKeys: 1}
}

// newServices wires the use cases. One Hub is shared by the job service (as
// its Waker) and the dispatcher, so queued jobs and cancel requests wake the
// long polls and passive loops of their worker. The remotes service checks
// the remotes of new jobs (jobs.RemoteChecker) and resolves them with their
// credentials when the dispatcher builds a job's spec (dispatch.Specs).
func newServices(cfg controllerconfig.Config, st store, cipher *secrets.Keyring, log *slog.Logger) (*services, error) {
	hub := dispatch.NewHub()
	clk := clock.System{}
	ids := idgen.UUIDv7{}

	backends, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		return nil, fmt.Errorf("backend registry: %w", err)
	}
	rs, err := remotes.New(remotes.Deps{
		Remotes: st, Cipher: cipher, Clock: clk, IDs: ids, Backends: backends,
		Logger: log.With("component", "remotes"),
	}, remotes.Config{})
	if err != nil {
		return nil, fmt.Errorf("remotes service: %w", err)
	}
	kinds, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		return nil, fmt.Errorf("job kinds: %w", err)
	}
	js, err := jobs.New(jobs.Deps{
		Jobs: st, Samples: st, Clock: clk, IDs: ids,
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: hub, Remotes: rs,
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
	specs, err := dispatch.NewSpecs(rs, kinds)
	if err != nil {
		return nil, err
	}
	disp, err := dispatch.New(dispatch.Deps{
		Jobs: js, Workers: ws, Clock: clk, Hub: hub, Specs: specs,
		Logger: log.With("component", "dispatch"),
	}, dispatch.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	us, err := newUsers(cfg.Auth, st, clk, ids, log)
	if err != nil {
		return nil, err
	}
	wake := make(chan struct{}, 1)
	return &services{
		users: us, remotes: rs, backends: backends, kinds: kinds,
		jobs: js, workers: ws, disp: disp, wake: wake,
		adminWorkers: &wakingWorkers{Service: ws, wake: wake},
	}, nil
}

// newUsers wires the users service: argon2id passwords (internal/adapters/
// password) and the login and setup rate limits.
func newUsers(cfg controllerconfig.Auth, st store, clk clock.System, ids idgen.UUIDv7, log *slog.Logger) (*users.Service, error) {
	hasher, err := password.New(password.Config{})
	if err != nil {
		return nil, fmt.Errorf("password hasher: %w", err)
	}
	ipLimit, userLimit, setupLimit := loginLimits()
	var limiters [3]*ratelimit.Limiter
	for i, c := range []ratelimit.Config{ipLimit, userLimit, setupLimit} {
		if limiters[i], err = ratelimit.New(c, clk); err != nil {
			return nil, err
		}
	}
	us, err := users.New(users.Deps{
		Users: st, Sessions: st, Tokens: st, Hasher: hasher,
		Limiters: users.Limiters{ClientIP: limiters[0], Username: limiters[1], Setup: limiters[2]},
		Clock:    clk, IDs: ids, Random: rand.Reader,
		Logger: log.With("component", "users"),
	}, users.Config{SessionIdleTimeout: cfg.SessionIdleTimeout, SessionMaxAge: cfg.SessionMaxAge})
	if err != nil {
		return nil, fmt.Errorf("users service: %w", err)
	}
	return us, nil
}

// setupCodeMessage is logged (at WARN, with the code as "setup_code") at
// start while no user exists.
const setupCodeMessage = "first-run setup: open the web UI and enter this setup code"

// armSetup prepares the first-run setup while no user exists: it arms the
// preset auth.setup_code or a new random code (a new one on every start,
// until the first admin exists) and logs where to enter it. A generated
// code is logged; a preset one is not repeated in the log.
func armSetup(ctx context.Context, cfg controllerconfig.Auth, us *users.Service, log *slog.Logger) error {
	required, err := us.SetupRequired(ctx)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	if preset := cfg.SetupCode.Reveal(); preset != "" {
		if err := us.ArmSetup(preset); err != nil {
			return fmt.Errorf("auth.setup_code: %w", err)
		}
		log.Warn("first-run setup: open the web UI and enter the setup code configured in auth.setup_code (SYNCR_SETUP_CODE)")
		return nil
	}
	code, err := users.NewSetupCode(rand.Reader)
	if err != nil {
		return err
	}
	if err := us.ArmSetup(code); err != nil {
		return err
	}
	log.Warn(setupCodeMessage, "setup_code", code)
	return nil
}

// seedDemo fills the in-memory store with the demo data of dev.seed
// (configuration validation allows it only with dev.memory_store).
func seedDemo(ctx context.Context, st store, svc *services, log *slog.Logger) (*devseed.Seed, error) {
	seed, err := devseed.Run(ctx, devseed.Deps{
		Users: svc.users, Remotes: svc.remotes, Workers: svc.workers, Jobs: svc.jobs, Kinds: svc.kinds,
		Store: st, Clock: clock.System{}, Logger: log.With("component", "devseed"),
	})
	if err != nil {
		return nil, fmt.Errorf("dev.seed: %w", err)
	}
	return seed, nil
}

// sessionPurgeInterval is how often expired sessions are deleted.
const sessionPurgeInterval = time.Hour

// purgeSessions deletes expired sessions now and every interval until ctx
// ends.
func purgeSessions(ctx context.Context, us *users.Service, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := us.PurgeExpired(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("session purge failed", "error", err.Error())
		case n > 0:
			log.Info("expired sessions purged", "sessions", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// apiConfig is the configuration of the API adapter derived from cfg.
type apiConfig struct {
	cookies  api.CookieConfig
	install  api.InstallConfig
	clientIP *clientip.Resolver
}

// newAPIConfig checks and derives the API adapter's settings: the session
// cookie, the worker install commands (http.public_url,
// workers.download_base_url) and the client IP of login and setup attempts
// (http.client_ip_header behind http.trusted_proxies).
func newAPIConfig(cfg controllerconfig.Config) (apiConfig, error) {
	trusted, err := controllerconfig.ParseTrustedProxies(cfg.HTTP.TrustedProxies)
	if err != nil {
		return apiConfig{}, err
	}
	c := apiConfig{
		cookies:  api.CookieConfig{Secure: api.CookieSecure(cfg.Auth.CookieSecure), PublicURL: cfg.HTTP.PublicURL},
		install:  api.InstallConfig{PublicURL: cfg.HTTP.PublicURL, DownloadBaseURL: cfg.Workers.DownloadBaseURL},
		clientIP: clientip.New(cfg.HTTP.ClientIPHeader, trusted),
	}
	if err := c.cookies.Validate(); err != nil {
		return apiConfig{}, fmt.Errorf("auth.cookie_secure: %w", err)
	}
	if err := c.install.Validate(); err != nil {
		return apiConfig{}, fmt.Errorf("worker install commands (http.public_url, workers.download_base_url): %w; "+
			"workers accept a controller URL with https, or http on a loopback address only", err)
	}
	return c, nil
}

// newHandler mounts the API (api.NewHandler) with the web UI and the worker
// link (ControllerLinkService, EnrollmentService) on one mux. lifetime ends
// the API's watch streams and the link's long polls.
func newHandler(lifetime context.Context, cfg controllerconfig.Config, ac apiConfig, svc *services, info version.Info, log *slog.Logger) (http.Handler, error) {
	auth, err := api.NewSessionAuth(svc.users, ac.cookies)
	if err != nil {
		return nil, err
	}
	apiHandler := api.NewHandler(api.Deps{
		BuildInfo:        info,
		Logger:           log.With("component", "api"),
		Auth:             auth,
		Identity:         svc.users,
		Users:            svc.users,
		Workers:          svc.adminWorkers,
		Jobs:             svc.jobs,
		Kinds:            svc.kinds,
		Remotes:          svc.remotes,
		Backends:         svc.backends,
		UI:               web.FS(),
		Cookies:          ac.cookies,
		ClientIP:         ac.clientIP.ClientIP,
		Install:          ac.install,
		BaseContext:      lifetime,
		RequestTimeout:   cfg.API.RequestTimeout,
		WatchMaxDuration: cfg.API.WatchMaxDuration,
		WatchKeepalive:   cfg.API.WatchKeepalive,
	})

	link, err := active.New(active.Options{
		Dispatcher: svc.disp,
		Workers:    svc.adminWorkers,
		Lifetime:   lifetime,
		ClientKey:  enrollClientKey(cfg.HTTP, ac.clientIP),
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
func enrollClientKey(cfg controllerconfig.HTTP, r *clientip.Resolver) active.ClientKeyFunc {
	if cfg.ClientIPHeader == "" {
		return nil // active.PeerIP
	}
	return func(peer connect.Peer, header http.Header) string { return r.ClientIP(peer.Addr, header) }
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
//  3. the background loops stop: the passive supervisor, the
//     dispatch-timeout sweeper, the session purge (and the dev seed's
//     animation);
//  4. the store is closed.
//
// If ready is non-nil it receives the listen address once the server
// accepts connections.
func serve(ctx context.Context, cfg controllerconfig.Config, info version.Info, log *slog.Logger, ready chan<- net.Addr) (err error) {
	for _, r := range cfg.RemovedSettings() {
		log.Warn("configuration: a removed setting is set and ignored", "setting", r.Key, "env", r.Env, "instead", r.Instead)
	}
	cipher, err := newKeyring(cfg.Secrets)
	if err != nil {
		return err
	}
	ac, err := newAPIConfig(cfg)
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

	svc, err := newServices(cfg, st, cipher, log)
	if err != nil {
		return err
	}
	var seed *devseed.Seed
	if cfg.Dev.Seed {
		if seed, err = seedDemo(ctx, st, svc, log); err != nil {
			return err
		}
	}
	if err := armSetup(ctx, cfg.Auth, svc.users, log); err != nil {
		return fmt.Errorf("first-run setup: %w", err)
	}
	if !ac.cookies.SecureFor(certs != nil) {
		log.Warn("session cookies are not marked Secure, so browsers also send them over plain HTTP: " +
			"behind a TLS-terminating reverse proxy set http.public_url (SYNCR_PUBLIC_URL) to its https:// URL, " +
			"or auth.cookie_secure: always; serve TLS directly with http.tls_cert_file and http.tls_key_file")
	}

	// lifetime is the server's lifetime: its end (right after ctx's, once
	// the step is logged) is what starts the graceful shutdown.
	lifetime, endLifetime := context.WithCancel(context.WithoutCancel(ctx))
	defer endLifetime()
	handler, err := newHandler(lifetime, cfg, ac, svc, info, log)
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
	background.Go(func() { purgeSessions(bgCtx, svc.users, sessionPurgeInterval, log.With("component", "users")) })
	if seed != nil {
		background.Go(func() { seed.Animate(bgCtx) })
	}
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
		"store", storeKind(cfg), "public_url", cfg.HTTP.PublicURL)
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
