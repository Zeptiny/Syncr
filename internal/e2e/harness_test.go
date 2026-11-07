package e2e

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	cactive "github.com/Zeptiny/Syncr/internal/controller/dispatch/active"
	cpassive "github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/tlscert"
	wactive "github.com/Zeptiny/Syncr/internal/worker/transport/active"
	wpassive "github.com/Zeptiny/Syncr/internal/worker/transport/passive"
)

// waitTimeout bounds every wait for an asynchronous outcome.
const waitTimeout = 10 * time.Second

// ---------------------------------------------------------------------------
// Controller
// ---------------------------------------------------------------------------

// controller is a controller process over an in-memory store, served by an
// httptest TLS server whose handler can be swapped: restart rebuilds every
// service over the same store, like a process restart behind the same
// address.
type controller struct {
	t      *testing.T
	store  *memstore.Store
	cipher *secrets.Keyring
	ids    *idgen.Sequential
	clock  *clock.Fake
	cfg    dispatch.Config

	srv     *httptest.Server
	handler atomic.Pointer[http.Handler]

	mu       sync.Mutex
	hub      *dispatch.Hub
	jobs     *jobs.Service
	workers  *workers.Service
	disp     *dispatch.Service
	wake     chan struct{}
	stop     context.CancelFunc
	running  sync.WaitGroup
	restarts int
}

func newController(t *testing.T) *controller {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	_, _ = rand.Read(key.Bytes)
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	c := &controller{
		t: t, store: memstore.New(), cipher: cipher, ids: &idgen.Sequential{},
		clock: clock.NewFake(time.Now()),
		cfg: dispatch.Config{
			MaxWait: 5 * time.Second,
			Passive: dispatch.PassiveConfig{
				BusyInterval: 50 * time.Millisecond, IdleInterval: 200 * time.Millisecond,
				MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
			},
		},
	}
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*c.handler.Load()).ServeHTTP(w, r)
	}))
	c.srv.Config.ReadHeaderTimeout = 5 * time.Second
	c.srv.Config.ReadTimeout = time.Second // shorter than a long poll on purpose
	c.srv.Config.WriteTimeout = time.Second
	c.start()
	c.srv.StartTLS()
	t.Cleanup(func() {
		c.shutdown()
		c.srv.Close()
	})
	return c
}

// start builds the services and the transports and runs the background
// loops (sweep, passive supervisor).
func (c *controller) start() {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	c.hub = dispatch.NewHub()
	c.jobs, err = jobs.New(jobs.Deps{
		Jobs: c.store, Samples: c.store, Clock: c.clock, IDs: c.ids,
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: c.hub,
	}, jobs.Config{SampleInterval: time.Millisecond})
	if err != nil {
		c.t.Fatal(err)
	}
	c.workers, err = workers.New(workers.Deps{
		Workers: c.store, Tokens: c.store, Cipher: c.cipher, Clock: c.clock, IDs: c.ids,
		Jobs: c.jobs, Random: rand.Reader,
	}, workers.Config{})
	if err != nil {
		c.t.Fatal(err)
	}
	c.disp, err = dispatch.New(dispatch.Deps{Jobs: c.jobs, Workers: c.workers, Specs: specBuilder{}, Clock: c.clock, Hub: c.hub}, c.cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.stop = cancel
	link, err := cactive.New(cactive.Options{
		Dispatcher: c.disp, Workers: c.workers, Lifetime: ctx,
		EnrollLimiter: cactive.NewRateLimiter(cactive.RateLimit{Every: time.Millisecond, Burst: 1000}, nil),
	})
	if err != nil {
		c.t.Fatal(err)
	}
	mux := http.NewServeMux()
	link.Mount(mux)
	var h http.Handler = mux
	c.handler.Store(&h)

	c.wake = make(chan struct{}, 1)
	sup, err := cpassive.NewSupervisor(cpassive.SupervisorOptions{
		Workers: c.workers, NewLoop: cpassive.DispatchLoops(c.disp), Wake: c.wake,
		Resync: 100 * time.Millisecond, MinRestart: 20 * time.Millisecond, MaxRestart: 200 * time.Millisecond,
		Client: cpassive.ClientConfig{Timeout: 5 * time.Second},
	})
	if err != nil {
		c.t.Fatal(err)
	}
	c.running.Go(func() { _ = sup.Run(ctx) })
	c.running.Go(func() { _ = c.disp.Run(ctx) })
}

// shutdown stops the background loops and ends the long polls.
func (c *controller) shutdown() {
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	stop()
	c.running.Wait()
}

// restart simulates a controller process restart: the old services and
// transports stop, connections are cut, and new ones are built over the
// same store, behind the same address.
func (c *controller) restart() {
	c.t.Helper()
	c.shutdown()
	var down http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "restarting", http.StatusServiceUnavailable)
	})
	c.handler.Store(&down)
	c.srv.CloseClientConnections()
	time.Sleep(50 * time.Millisecond)
	c.restarts++
	c.start()
}

// svc returns the current services.
func (c *controller) svc() (*jobs.Service, *workers.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jobs, c.workers
}

// rootCAs trusts the test server's certificate.
func (c *controller) rootCAs() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.srv.Certificate())
	return pool
}

// httpClient is a client that trusts the test server, optionally through
// a wrapping round tripper.
func (c *controller) httpClient(wrap func(http.RoundTripper) http.RoundTripper) *http.Client {
	tr := c.srv.Client().Transport
	if wrap != nil {
		tr = wrap(tr)
	}
	return &http.Client{Transport: tr}
}

// enrollToken creates an enrollment token.
func (c *controller) enrollToken() string {
	c.t.Helper()
	_, ws := c.svc()
	g, err := ws.CreateEnrollmentToken(context.Background(), 0)
	if err != nil {
		c.t.Fatal(err)
	}
	return g.Token
}

// enroll enrolls a worker over HTTP, as syncr-worker setup does.
func (c *controller) enroll(req *syncrv1.EnrollRequest) (*syncrv1.EnrollResponse, error) {
	cl := syncrv1connect.NewEnrollmentServiceClient(c.srv.Client(), c.srv.URL)
	resp, err := cl.Enroll(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (c *controller) enrollActive(name string) (domain.WorkerID, string) {
	c.t.Helper()
	resp, err := c.enroll(&syncrv1.EnrollRequest{EnrollmentToken: c.enrollToken(), Name: name, Os: "linux", Arch: "amd64"})
	if err != nil {
		c.t.Fatalf("enroll: %v", err)
	}
	id, err := domain.ParseWorkerID(resp.GetWorkerId())
	if err != nil {
		c.t.Fatal(err)
	}
	return id, resp.GetWorkerSecret()
}

// testOwner owns the test jobs.
var testOwner = domain.NewUserID()

// specBuilder is the dispatcher's SpecBuilder in these tests: a job's
// "definition" is the protojson of the very spec to send (see run).
//
// m3: pending track C: real definitions are built by job kinds from the
// owner's resolved remotes; these link tests only need the spec delivered.
type specBuilder struct{}

func (specBuilder) Build(_ context.Context, j jobs.Job) (*syncrv1.JobSpec, error) {
	spec := new(syncrv1.JobSpec)
	if err := protojson.Unmarshal(j.Definition, spec); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidArgument, err)
	}
	return spec, nil
}

// run queues a job on worker w.
func (c *controller) run(w domain.WorkerID, spec *syncrv1.JobSpec) domain.JobID {
	c.t.Helper()
	raw, err := protojson.Marshal(spec)
	if err != nil {
		c.t.Fatal(err)
	}
	js, _ := c.svc()
	j, err := js.Create(context.Background(), jobs.CreateParams{WorkerID: w, OwnerID: testOwner, Kind: "rclone_transfer", Definition: raw})
	if err != nil {
		c.t.Fatal(err)
	}
	return j.ID
}

func (c *controller) job(id domain.JobID) jobs.Job {
	c.t.Helper()
	js, _ := c.svc()
	j, err := js.Get(context.Background(), id)
	if err != nil {
		c.t.Fatal(err)
	}
	return j
}

func (c *controller) cancel(id domain.JobID) {
	c.t.Helper()
	js, _ := c.svc()
	if _, err := js.Cancel(context.Background(), id); err != nil {
		c.t.Fatal(err)
	}
}

// waitJob waits until the job satisfies ok.
func (c *controller) waitJob(id domain.JobID, what string, ok func(jobs.Job) bool) jobs.Job {
	c.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		j := c.job(id)
		if ok(j) {
			return j
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("job %s: timed out waiting for %s; status %s, error %+v", id, what, j.Status, j.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *controller) waitStatus(id domain.JobID, want domain.JobStatus) jobs.Job {
	c.t.Helper()
	return c.waitJob(id, string(want), func(j jobs.Job) bool { return j.Status == want })
}

// waitWorker waits until the worker satisfies ok.
func (c *controller) waitWorker(id domain.WorkerID, what string, ok func(workers.Worker) bool) workers.Worker {
	c.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		_, ws := c.svc()
		w, err := ws.Get(context.Background(), id)
		if err != nil {
			c.t.Fatal(err)
		}
		if ok(w) {
			return w
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("worker %s: timed out waiting for %s; link %+v", id, what, w.Link)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wakeSupervisor asks the passive supervisor to resync now.
func (c *controller) wakeSupervisor() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

// fakeRunner runs jobs without tools. A job emits a progress sample, then
// blocks until released (success) or cancelled; jobs not held succeed at
// once. It counts runs per job.
type fakeRunner struct {
	mu      sync.Mutex
	runs    map[string]int
	hold    map[string]chan struct{}
	started chan string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{runs: map[string]int{}, hold: map[string]chan struct{}{}, started: make(chan string, 1000)}
}

func (*fakeRunner) Kind() string { return "rclone_transfer" }

// holdJob makes job block until release(job).
func (f *fakeRunner) holdJob(job domain.JobID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold[job.String()] = make(chan struct{})
}

func (f *fakeRunner) release(job domain.JobID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.hold[job.String()])
}

func (f *fakeRunner) runCount(job domain.JobID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[job.String()]
}

// waitStarted waits until the runner was called for job.
func (f *fakeRunner) waitStarted(t *testing.T, job domain.JobID) {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		if f.runCount(job) > 0 {
			return
		}
		select {
		case <-f.started:
		case <-deadline:
			t.Fatalf("job %s never started", job)
		}
	}
}

func (f *fakeRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	f.mu.Lock()
	f.runs[job.ID]++
	hold := f.hold[job.ID]
	f.mu.Unlock()
	f.started <- job.ID
	emit(runner.Progress{Bytes: 10, Files: 1})
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return runner.Result{ExitCode: -1, Stats: runner.Progress{Bytes: 10, Files: 1}}, context.Cause(ctx)
		}
	}
	return runner.Result{Stats: runner.Progress{Bytes: 100, Files: 2}}, nil
}

func testSpec() *syncrv1.JobSpec {
	return &syncrv1.JobSpec{
		Remotes: []*syncrv1.RemoteDefinition{{Alias: "s3", BackendType: "s3", Secrets: map[string]string{"secret_access_key": "hunter2"}}},
		Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
			Source:      &syncrv1.Location{Path: "/data"},
			Destination: &syncrv1.Location{Remote: "s3", Path: "bucket"},
		}},
	}
}

// newAgent builds an agent over rn.
func newAgent(t *testing.T, rn runner.Runner) (*agent.Agent, *linkcore.Core) {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register(rn); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Runners: reg, ProgressInterval: 20 * time.Millisecond, MaxConcurrentJobs: 4,
		Identity: agent.Identity{WorkerVersion: "test", OS: "linux", Arch: "amd64", Hostname: "e2e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a, linkcore.New(a, a.Outbox(), nil)
}

// activeWorker is a worker process in active mode.
type activeWorker struct {
	agent     *agent.Agent
	core      *linkcore.Core
	transport *wactive.Transport
	cancel    context.CancelFunc
	done      chan struct{}
}

// activeOptions are fast transport timings for tests.
func activeOptions(c *controller, id domain.WorkerID, secret string) wactive.Options {
	return wactive.Options{
		ControllerURL: c.srv.URL, WorkerID: id.String(), Secret: secret, RootCAs: c.rootCAs(),
		RequestTimeout: 5 * time.Second, PollWait: 3 * time.Second, PollGrace: 2 * time.Second,
		ReportInterval: 20 * time.Millisecond, Heartbeat: 300 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
	}
}

// startActive starts a worker process with its own agent.
func startActive(t *testing.T, rn runner.Runner, opts wactive.Options) *activeWorker {
	t.Helper()
	a, core := newAgent(t, rn)
	tr, err := wactive.New(core, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &activeWorker{agent: a, core: core, transport: tr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		_ = tr.Run(ctx)
	}()
	t.Cleanup(w.kill)
	return w
}

// kill stops the transport without a final flush (a crash, as far as the
// controller can tell).
func (w *activeWorker) kill() {
	w.cancel()
	<-w.done
}

// shutdown is the graceful shutdown of the composition root.
func (w *activeWorker) shutdown(t *testing.T) error {
	t.Helper()
	w.kill()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := w.agent.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	return w.transport.Drain(ctx)
}

// passiveWorker is a worker process in passive mode.
type passiveWorker struct {
	agent       *agent.Agent
	server      *wpassive.Server
	url         string
	fingerprint string
	served      chan error
}

// startPassive starts a passive worker with a new self-signed certificate,
// serving TLS on a free port.
func startPassive(t *testing.T, rn runner.Runner, secret string) *passiveWorker {
	t.Helper()
	certFile, keyFile, _ := selfSigned(t)
	return startPassiveOn(t, rn, certFile, keyFile, secret, listen(t))
}

// listen opens a listener on a free loopback port.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startPassiveOn starts a passive worker serving TLS on ln.
func startPassiveOn(t *testing.T, rn runner.Runner, certFile, keyFile, secret string, ln net.Listener) *passiveWorker {
	t.Helper()
	a, core := newAgent(t, rn)
	srv, err := wpassive.New(core, wpassive.Options{Addr: ln.Addr().String(), CertFile: certFile, KeyFile: keyFile, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	w := &passiveWorker{agent: a, server: srv, url: "https://" + ln.Addr().String(), fingerprint: srv.Fingerprint(), served: make(chan error, 1)}
	go func() { w.served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return w
}

// pairingSecret returns a random passive pairing secret.
func pairingSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "pair_" + hex.EncodeToString(b)
}

// selfSigned writes a self-signed certificate for a worker, generated like
// syncr-worker setup does (the controller pins it), and returns the file
// paths and the fingerprint.
func selfSigned(t *testing.T) (certFile, keyFile, fingerprint string) {
	t.Helper()
	pair, err := tlscert.Generate(tlscert.Options{Hostname: "syncr-worker"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := tlscert.Write(certFile, keyFile, pair, true); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, pair.Fingerprint
}

// ---------------------------------------------------------------------------
// Faults
// ---------------------------------------------------------------------------

// errDropped is returned by flakyTransport for a lost request or response.
var errDropped = errors.New("flaky network: dropped")

// flakyTransport loses requests (never sent) and responses (sent and
// handled, but the answer is lost) at random, from a seeded generator.
// The loops' calls interleave freely, so which calls are lost still
// varies between runs.
type flakyTransport struct {
	next             http.RoundTripper
	dropReq, dropRes float64

	mu   sync.Mutex
	rng  *mrand.Rand
	lost atomic.Int64
}

func (f *flakyTransport) roll() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rng.Float64()
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.roll() < f.dropReq {
		f.lost.Add(1)
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: request", errDropped)
	}
	resp, err := f.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if f.roll() < f.dropRes {
		f.lost.Add(1)
		// The server handled the call; lose its answer.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: response", errDropped)
	}
	return resp, nil
}
