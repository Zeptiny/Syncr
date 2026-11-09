package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/internal/adapters/api"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	notifybuiltin "github.com/Zeptiny/Syncr/internal/adapters/notify/builtin"
	"github.com/Zeptiny/Syncr/internal/adapters/password"
	"github.com/Zeptiny/Syncr/internal/adapters/postgres"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/active"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/controller/scheduler"
	"github.com/Zeptiny/Syncr/internal/devseed"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/notify"
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
	jobs.ActivityRepository
	users.Repository
	users.SessionRepository
	users.APITokenRepository
	remotes.Repository
	schedules.Repository
	contacts.Repository
	notifications.Repository
	restic.Repository
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

	// channels are the notification channel types (notify.DryRun under
	// dev.seed); contacts and notifications use them.
	channels      *notify.Registry
	contacts      *contacts.Service
	notifications *notifications.Service
	schedules     *schedules.Service
	scheduler     *scheduler.Scheduler

	// describer describes jobs for their notifications (the outbox's and
	// the dev seed's).
	describer jobDescriber
}

// loginLimits are the rate limits of credential checks: per client IP
// burst 10, then 1 every 30 s; per username burst 5, then 1 a minute;
// setup attempts burst 5, then 1 a minute, globally.
func loginLimits() (clientIP, username, setup ratelimit.Config) {
	return ratelimit.Config{Burst: 10, Every: 30 * time.Second, MaxKeys: 10000},
		ratelimit.Config{Burst: 5, Every: time.Minute, MaxKeys: 10000},
		ratelimit.Config{Burst: 5, Every: time.Minute, MaxKeys: 1}
}

// sendTestLimit is the rate limit of contacts' test sends, per user.
func sendTestLimit() ratelimit.Config {
	return ratelimit.Config{Burst: contacts.TestBurst, Every: contacts.TestEvery, MaxKeys: 10000}
}

// newServices wires the use cases. One Hub is shared by the job service (as
// its Waker) and the dispatcher, so queued jobs and cancel requests wake the
// long polls and passive loops of their worker. The remotes service checks
// the remotes of new jobs (jobs.RemoteChecker) and resolves them with their
// credentials when the dispatcher builds a job's spec (dispatch.Specs).
//
// A finished job wakes the notification outbox and the scheduler
// (jobFinished): the outbox plans its notifications, and runs queued
// behind it may launch. One scheduler.Signal is shared by the schedules
// service (a saved schedule or Run now wakes it) and the scheduler, which
// breaks their construction cycle.
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
	describer := jobDescriber{kinds: kinds, remotes: rs}
	channels, err := newChannels(cfg, log)
	if err != nil {
		return nil, err
	}
	testLimit, err := ratelimit.New(sendTestLimit(), clk)
	if err != nil {
		return nil, err
	}
	cs, err := contacts.New(contacts.Deps{
		Contacts: st, Channels: channels, Cipher: cipher, Limiter: testLimit, Clock: clk, IDs: ids,
		Logger: log.With("component", "contacts"),
	}, contacts.Config{SendTimeout: cfg.Notify.SendTimeout})
	if err != nil {
		return nil, fmt.Errorf("contacts service: %w", err)
	}
	ns, err := notifications.New(notifications.Deps{
		Repo: st, Channels: channels, Contacts: cs, Admins: adminChecker{users: st}, Jobs: describer, Clock: clk, IDs: ids, PublicURL: cfg.HTTP.PublicURL,
		Logger: log.With("component", "notifications"),
	}, notifications.Config{
		MaxAttempts: cfg.Notify.MaxAttempts, Concurrency: cfg.Notify.Concurrency,
		SendTimeout: cfg.Notify.SendTimeout, Retention: cfg.Notify.Retention,
	})
	if err != nil {
		return nil, fmt.Errorf("notifications service: %w", err)
	}
	sig := scheduler.NewSignal()
	js, err := jobs.New(jobs.Deps{
		Jobs: st, Samples: st, Activity: st, Clock: clk, IDs: ids,
		Broker: broker.New(), Notifier: jobFinished{ns, sig}, Waker: hub, Remotes: rs,
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
	// m5: the integration step passes the restic service as the
	// repositories (dispatch.Repos); without it restic jobs fail with
	// repository_unavailable.
	specs, err := dispatch.NewSpecs(rs, nil, kinds)
	if err != nil {
		return nil, err
	}
	dcfg := dispatch.DefaultConfig()
	dcfg.LostAfter = cfg.Workers.LostAfter
	disp, err := dispatch.New(dispatch.Deps{
		Jobs: js, Workers: ws, Clock: clk, Hub: hub, Specs: specs,
		Logger: log.With("component", "dispatch"),
	}, dcfg)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	us, err := newUsers(cfg.Auth, st, clk, ids, log)
	if err != nil {
		return nil, err
	}
	ss, err := schedules.New(schedules.Deps{
		Repo: st, Remotes: rs, Contacts: cs, Workers: workerChecker{ws}, Waker: sig, Clock: clk, IDs: ids,
		Logger: log.With("component", "schedules"),
	}, schedules.Config{})
	if err != nil {
		return nil, fmt.Errorf("schedules service: %w", err)
	}
	sched, err := scheduler.New(scheduler.Deps{
		Schedules: notifyingSchedules{Service: ss, outbox: ns}, Jobs: js, Workers: ws, Kinds: kinds, Owners: us,
		IDs: ids, Clock: clk, Signal: sig, Logger: log.With("component", "scheduler"),
	}, scheduler.Config{Tick: cfg.Schedules.Tick})
	if err != nil {
		return nil, fmt.Errorf("scheduler: %w", err)
	}
	wake := make(chan struct{}, 1)
	return &services{
		users: us, remotes: rs, backends: backends, kinds: kinds, describer: describer,
		jobs: js, workers: ws, disp: disp, wake: wake,
		adminWorkers: &wakingWorkers{Service: ws, wake: wake},
		channels:     channels, contacts: cs, notifications: ns, schedules: ss, scheduler: sched,
	}, nil
}

// newChannels returns the built-in notification channels. Their HTTP
// client and SMTP dialer check every connected address against
// notify.network_policy and notify.allowed_networks (netguard), with
// notify.send_timeout. Under dev.seed they only log what they would send
// (notify.DryRun), so demo contacts never reach their made-up targets.
func newChannels(cfg controllerconfig.Config, log *slog.Logger) (*notify.Registry, error) {
	policy, err := cfg.Notify.Policy()
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "" // the channels fall back to "localhost"
	}
	timeout := cfg.Notify.SendTimeout
	reg, err := notify.NewRegistry(notifybuiltin.Channels(notifybuiltin.Deps{
		HTTP: policy.HTTPClient(timeout), Dial: policy.Dialer(timeout), Hostname: hostname,
	})...)
	if err != nil {
		return nil, fmt.Errorf("notification channels: %w", err)
	}
	if cfg.Dev.Seed {
		reg = notify.DryRun(reg, log.With("component", "notify"))
	}
	return reg, nil
}

// jobFinished is the jobs service's Notifier: a job that reached a
// terminal status wakes the notification outbox (to plan its
// notifications) and the scheduler (runs queued behind the job may launch
// now). Both are non-blocking; their periodic ticks are the backstop.
type jobFinished struct {
	outbox    *notifications.Service
	scheduler *scheduler.Signal
}

// JobFinished implements jobs.Notifier.
func (f jobFinished) JobFinished(jobs.Job) {
	f.outbox.Wake()
	f.scheduler.Wake()
}

// notifyingSchedules is the schedules service as the scheduler uses it: a
// fire (which may record a run missed for overlap at once) and a run that
// ends missed or failed wake the notification outbox, so the notification
// goes out now instead of at the outbox's next tick.
type notifyingSchedules struct {
	*schedules.Service
	outbox interface{ Wake() }
}

var _ scheduler.Schedules = notifyingSchedules{}

// FireDue fires the due schedules and wakes the outbox if any fired.
func (s notifyingSchedules) FireDue(ctx context.Context) (int, error) {
	n, err := s.Service.FireDue(ctx)
	if n > 0 {
		s.outbox.Wake()
	}
	return n, err
}

// EndRun ends a run and wakes the outbox if it ended missed or failed.
func (s notifyingSchedules) EndRun(ctx context.Context, run schedules.Run, status schedules.RunStatus, reason, msg string, pause bool) (schedules.Run, error) {
	r, err := s.Service.EndRun(ctx, run, status, reason, msg, pause)
	if err == nil && (status == schedules.RunMissed || status == schedules.RunFailed) {
		s.outbox.Wake()
	}
	return r, err
}

// adminChecker is the notification outbox's notifications.AdminChecker: it
// reads a user's role from the users store.
type adminChecker struct {
	users interface {
		GetUser(ctx context.Context, id domain.UserID) (users.User, error)
	}
}

var _ notifications.AdminChecker = adminChecker{}

// IsAdmin implements notifications.AdminChecker.
func (a adminChecker) IsAdmin(ctx context.Context, id domain.UserID) (bool, error) {
	u, err := a.users.GetUser(ctx, id)
	if err != nil {
		return false, fmt.Errorf("get user %s: %w", id, err)
	}
	return u.Role == domain.RoleAdmin, nil
}

// remoteNames is the remotes service as jobDescriber uses it.
type remoteNames interface {
	Names(ctx context.Context, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Ref, error)
}

// jobDescriber is the notification outbox's notifications.JobDescriber: it
// decodes a job's stored definition, names the remotes it refers to with
// remotes.Names (deleted ones included, so a job whose remote was deleted
// since still names it) and describes it with its kind
// (jobkind.Registry.Describe), as the web UI's jobs list does. Only remotes
// of the job's owner are named; any other (which a checked definition
// cannot refer to) is an "Unknown remote".
type jobDescriber struct {
	kinds   *jobkind.Registry
	remotes remoteNames
}

var _ notifications.JobDescriber = jobDescriber{}

// DescribeJob implements notifications.JobDescriber. A definition that
// cannot be decoded, or is not of kind, gets no description; a definition
// the current rules reject is described without remote names.
//
// m5: the integration step names the owner's repositories too and fills
// KindLabel (kinds.Label) and Details (kinds.Summarize of output).
func (d jobDescriber) DescribeJob(ctx context.Context, owner domain.UserID, kind string, definition, _ []byte) (notifications.JobDescription, error) {
	def, err := jobkind.Unmarshal(definition)
	if err != nil || jobkind.KindOf(def) != kind {
		// Not an error to retry: the definition will not decode later.
		return notifications.JobDescription{}, nil
	}
	names := map[domain.RemoteID]string{}
	if _, refs, err := d.kinds.Check(def); err == nil && len(refs.RemoteIDs) > 0 {
		rs, err := d.remotes.Names(ctx, refs.RemoteIDs)
		if err != nil {
			return notifications.JobDescription{}, fmt.Errorf("describe a %s job: %w", kind, err)
		}
		for id, r := range rs {
			if r.OwnerID == owner {
				names[id] = r.Name
			}
		}
	}
	return notifications.JobDescription{Text: d.kinds.Describe(def, jobkind.Names{Remotes: names})}, nil
}

// workerChecker adapts the workers service to schedules.WorkerChecker: its
// Get fails with ErrNotFound for a missing or removed worker.
type workerChecker struct{ ws *workers.Service }

// CheckWorker implements schedules.WorkerChecker.
func (c workerChecker) CheckWorker(ctx context.Context, id domain.WorkerID) error {
	_, err := c.ws.Get(ctx, id)
	return err
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
		Contacts: svc.contacts, Channels: svc.channels, Describer: svc.describer,
		Store: st, Clock: clock.System{}, Logger: log.With("component", "devseed"),
	})
	if err != nil {
		return nil, fmt.Errorf("dev.seed: %w", err)
	}
	return seed, nil
}

// backgroundStoppedMessage is logged in shutdown step 3 (see serve).
const backgroundStoppedMessage = "shutdown: background loops stopped (dispatch sweeper, passive supervisor, scheduler, notifications, purges)"

// sessionPurgeInterval is how often expired sessions and API tokens are
// deleted.
const sessionPurgeInterval = time.Hour

// purgeSessions deletes expired sessions and API tokens (and the tokens
// of disabled users) now and every interval until ctx ends.
func purgeSessions(ctx context.Context, us *users.Service, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := us.PurgeExpired(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("session purge failed", "error", err.Error())
		case n > 0:
			log.Info("expired sessions and API tokens purged", "count", n)
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
		Schedules:        svc.schedules,
		Contacts:         svc.contacts,
		Deliveries:       svc.notifications,
		Channels:         svc.channels,
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
//  3. the background loops stop, all before the store closes: the
//     dispatch-timeout sweeper, the passive supervisor, the scheduler,
//     the notification outbox (it records the outcome of the sends in
//     flight first), the session and token purge (and the dev seed's
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
	var targets passive.Targets = svc.workers
	if seed != nil {
		targets = seededTargets{Targets: svc.workers, skip: seed.Workers}
	}
	sup, err := passive.NewSupervisor(passive.SupervisorOptions{
		Workers: targets,
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

	// Background loops outlive the HTTP server: they stop in step 3, in
	// any order, but all before the store closes.
	bgCtx, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackground()
	var background sync.WaitGroup
	background.Go(func() { _ = svc.disp.Run(bgCtx) })
	background.Go(func() { _ = sup.Run(bgCtx) })
	background.Go(func() { _ = svc.scheduler.Run(bgCtx) })
	background.Go(func() { _ = svc.notifications.Run(bgCtx) })
	background.Go(func() { purgeSessions(bgCtx, svc.users, sessionPurgeInterval, log.With("component", "users")) })
	if seed != nil {
		background.Go(func() { seed.Animate(bgCtx) })
	}
	stopLoops := func() {
		stopBackground()
		background.Wait()
		log.Info(backgroundStoppedMessage)
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

// seededTargets are the passive workers the supervisor links to on a
// seeded controller: not the demo workers (their URLs lead nowhere, and
// the seed's animation keeps them online), so a demo logs no link errors.
// Passive workers registered later get their links as usual.
type seededTargets struct {
	passive.Targets
	skip map[string]domain.WorkerID
}

// List lists the workers of f without the demo workers.
func (t seededTargets) List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error) {
	ws, err := t.Targets.List(ctx, f)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ws, func(w workers.Worker) bool {
		return slices.Contains(slices.Collect(maps.Values(t.skip)), w.ID)
	}), nil
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
