package active_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/active"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// env is a controller over an in-memory store behind an httptest server.
type env struct {
	t       *testing.T
	clock   *clock.Fake
	jobs    *jobs.Service
	workers *workers.Service
	disp    *dispatch.Service
	probes  *dispatch.ProbeBroker
	srv     *httptest.Server
	stop    context.CancelFunc
	// owner owns the queued jobs (it exists in the store).
	owner domain.Actor
}

type envOptions struct {
	maxWait      time.Duration
	link         active.Options
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	_, _ = rand.Read(key.Bytes)
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	store, ids := memstore.New(), &idgen.Sequential{}
	e := &env{t: t, clock: clock.NewFake(time.Now()), owner: domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleUser}}
	if err := store.CreateUser(context.Background(), users.User{ID: e.owner.UserID, Username: "owner", Role: domain.RoleUser, Version: 1}); err != nil {
		t.Fatal(err)
	}
	hub := dispatch.NewHub()
	e.jobs, err = jobs.New(jobs.Deps{
		Jobs: store, Samples: store, Clock: e.clock, IDs: ids,
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: hub,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.workers, err = workers.New(workers.Deps{
		Workers: store, Tokens: store, Cipher: cipher, Clock: e.clock, IDs: ids, Jobs: e.jobs, Random: rand.Reader,
	}, workers.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.probes, err = dispatch.NewProbeBroker(dispatch.ProbeBrokerDeps{Workers: e.workers, Hub: hub, Clock: e.clock}, dispatch.ProbeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	e.disp, err = dispatch.New(dispatch.Deps{
		Jobs: e.jobs, Workers: e.workers, Specs: specBuilder{}, Probes: e.probes, Clock: e.clock, Hub: hub,
	}, dispatch.Config{MaxWait: o.maxWait})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	opts := o.link
	opts.Dispatcher, opts.Workers, opts.Lifetime = e.disp, e.workers, ctx
	h, err := active.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	e.srv = httptest.NewUnstartedServer(mux)
	e.srv.Config.ReadHeaderTimeout = 5 * time.Second
	e.srv.Config.ReadTimeout = o.readTimeout
	e.srv.Config.WriteTimeout = o.writeTimeout
	e.srv.Start()
	t.Cleanup(func() {
		cancel()
		e.srv.Close()
	})
	return e
}

// enroll returns an active worker's credentials.
func (e *env) enroll() (domain.WorkerID, string) {
	e.t.Helper()
	return e.enroll2("w")
}

// enroll2 enrolls an active worker called name.
func (e *env) enroll2(name string) (domain.WorkerID, string) {
	e.t.Helper()
	g, err := e.workers.CreateEnrollmentToken(context.Background(), 0)
	if err != nil {
		e.t.Fatal(err)
	}
	cl := syncrv1connect.NewEnrollmentServiceClient(e.srv.Client(), e.srv.URL)
	resp, err := cl.Enroll(context.Background(), connect.NewRequest(&syncrv1.EnrollRequest{EnrollmentToken: g.Token, Name: name}))
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := domain.ParseWorkerID(resp.Msg.GetWorkerId())
	if err != nil {
		e.t.Fatal(err)
	}
	return id, resp.Msg.GetWorkerSecret()
}

func (e *env) client() syncrv1connect.ControllerLinkServiceClient {
	return syncrv1connect.NewControllerLinkServiceClient(e.srv.Client(), e.srv.URL)
}

func authed[T any](msg *T, id domain.WorkerID, secret string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", link.ActiveAuthorization(id.String(), secret))
	return r
}

func hello(seq int64) *syncrv1.Event {
	return &syncrv1.Event{Seq: seq, Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{Hostname: "h"}}}
}

// specBuilder sends every job as an empty rclone transfer: these transport
// tests only need some spec delivered (dispatch.Specs, which builds real
// specs from definitions, is tested in package dispatch).
type specBuilder struct{}

func (specBuilder) Build(context.Context, jobs.Job, workers.Info) (*syncrv1.JobSpec, error) {
	return &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}}, nil
}

func (e *env) queue(id domain.WorkerID) domain.JobID {
	e.t.Helper()
	j, err := e.jobs.Create(context.Background(), e.owner, jobs.CreateParams{
		WorkerID: id, Kind: "rclone_transfer", Definition: []byte(`{"rclone_transfer":{}}`),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j.ID
}

func TestReportPollAndStaleEpoch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{maxWait: time.Second})
	id, secret := e.enroll()
	cl := e.client()
	ctx := context.Background()

	resp, err := cl.Report(ctx, authed(&syncrv1.ReportRequest{Epoch: "e1", Events: []*syncrv1.Event{hello(1)}}, id, secret))
	if err != nil || resp.Msg.GetAckSeq() != 1 {
		t.Fatalf("report = %v, %v", resp, err)
	}
	job := e.queue(id)
	poll, err := cl.Poll(ctx, authed(&syncrv1.PollRequest{Epoch: "e1", Wait: durationpb.New(0)}, id, secret))
	if err != nil || len(poll.Msg.GetCommands()) != 1 || poll.Msg.GetCommands()[0].GetStartJob().GetJobId() != job.String() {
		t.Fatalf("poll = %v, %v", poll, err)
	}

	// A new process, then a delayed request of the old one.
	if _, err := cl.Report(ctx, authed(&syncrv1.ReportRequest{Epoch: "e2", Events: []*syncrv1.Event{hello(1)}}, id, secret)); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error {
			_, err := cl.Report(ctx, authed(&syncrv1.ReportRequest{Epoch: "e1"}, id, secret))
			return err
		},
		func() error {
			_, err := cl.Poll(ctx, authed(&syncrv1.PollRequest{Epoch: "e1"}, id, secret))
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("stale epoch: %v", err)
		}
	}
	if _, err := cl.Report(ctx, authed(&syncrv1.ReportRequest{Epoch: ""}, id, secret)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty epoch: %v", err)
	}
}

// TestCompleteProbe: a probe reaches an active worker through its long
// poll and its result comes back through CompleteProbe.
func TestCompleteProbe(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{maxWait: 5 * time.Second})
	id, secret := e.enroll()
	cl := e.client()
	ctx := context.Background()

	current := &syncrv1.Event{Seq: 1, Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{
		Hostname: "h", Features: link.Features(), ProbeOps: link.ProbeOps(),
	}}}
	if _, err := cl.Report(ctx, authed(&syncrv1.ReportRequest{Epoch: "e1", Events: []*syncrv1.Event{current}}, id, secret)); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		res *syncrv1.ProbeResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := e.probes.Probe(ctx, id, &syncrv1.RunProbe{
			ProbeId: "p1", Spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: "/data"}}},
		})
		done <- answer{res, err}
	}()
	// The long poll returns as soon as the probe is queued.
	poll, err := cl.Poll(ctx, authed(&syncrv1.PollRequest{Epoch: "e1", Wait: durationpb.New(5 * time.Second)}, id, secret))
	if err != nil || len(poll.Msg.GetCommands()) != 1 || poll.Msg.GetCommands()[0].GetRunProbe().GetSpec().GetListLocal().GetPath() != "/data" {
		t.Fatalf("poll = %v, %v", poll, err)
	}
	cmdID := poll.Msg.GetCommands()[0].GetId()

	result := &syncrv1.ProbeResult{
		ProbeId: "p1", Status: syncrv1.ProbeStatus_PROBE_STATUS_OK,
		Output: &syncrv1.ProbeResult_Listing{Listing: &syncrv1.ProbeListing{Path: "/data"}},
	}
	// Without credentials, and from a replaced process, nothing is taken.
	if _, err := cl.CompleteProbe(ctx, connect.NewRequest(&syncrv1.CompleteProbeRequest{Epoch: "e1", Result: result})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated: %v", err)
	}
	if _, err := cl.CompleteProbe(ctx, authed(&syncrv1.CompleteProbeRequest{Epoch: "e1"}, id, secret)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("no result: %v", err)
	}
	other, otherSecret := e.enroll2("other")
	if _, err := cl.Report(ctx, authed(&syncrv1.ReportRequest{Epoch: "o1", Events: []*syncrv1.Event{hello(1)}}, other, otherSecret)); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.CompleteProbe(ctx, authed(&syncrv1.CompleteProbeRequest{Epoch: "o1", Result: result}, other, otherSecret)); err != nil {
		t.Fatalf("another worker's result must be accepted and ignored: %v", err)
	}
	select {
	case a := <-done:
		t.Fatalf("another worker completed the probe: %v, %v", a.res, a.err)
	default:
	}

	if _, err := cl.Poll(ctx, authed(&syncrv1.PollRequest{Epoch: "e1", Wait: durationpb.New(0), Acks: []*syncrv1.CommandAck{{CommandId: cmdID}}}, id, secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.CompleteProbe(ctx, authed(&syncrv1.CompleteProbeRequest{Epoch: "e1", Result: result}, id, secret)); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.err != nil || a.res.GetListing().GetPath() != "/data" {
			t.Fatalf("probe = %v, %v", a.res, a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe did not return")
	}
	// Idempotent.
	if _, err := cl.CompleteProbe(ctx, authed(&syncrv1.CompleteProbeRequest{Epoch: "e1", Result: result}, id, secret)); err != nil {
		t.Fatalf("repeated result: %v", err)
	}
}

func TestUnauthenticated(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	id, secret := e.enroll()
	_, passive, err := func() (domain.WorkerID, workers.Worker, error) {
		w, err := e.workers.RegisterPassive(context.Background(), workers.PassiveParams{
			Name: "p", URL: "https://p.example", CertFingerprint: strings.Repeat("ab", 32), Secret: strings.Repeat("s", 40),
		})
		return w.ID, w, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	cl := e.client()
	for name, h := range map[string]string{
		"none":           "",
		"wrong secret":   link.ActiveAuthorization(id.String(), secret+"x"),
		"unknown worker": link.ActiveAuthorization(domain.NewWorkerID().String(), secret),
		"bad id":         link.ActiveAuthorization("x", secret),
		"passive worker": link.ActiveAuthorization(passive.ID.String(), strings.Repeat("s", 40)),
		"basic":          "Basic Zm9vOmJhcg==",
	} {
		req := connect.NewRequest(&syncrv1.PollRequest{Epoch: "e1"})
		if h != "" {
			req.Header().Set("Authorization", h)
		}
		_, err := cl.Poll(context.Background(), req)
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated || ce.Message() != "invalid worker credentials" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestPollWakesAndCaps: a job queued during a Poll is returned at once; a
// wait above the dispatcher's maximum is capped.
func TestPollWakesAndCaps(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{maxWait: 500 * time.Millisecond})
	id, secret := e.enroll()
	cl := e.client()

	start := time.Now()
	_, err := cl.Poll(context.Background(), authed(&syncrv1.PollRequest{Epoch: "e1", Wait: durationpb.New(time.Hour)}, id, secret))
	if took := time.Since(start); err != nil || took < 400*time.Millisecond || took > 3*time.Second {
		t.Fatalf("capped poll: %v after %v", err, took)
	}

	done := make(chan time.Time, 1)
	go func() {
		resp, err := cl.Poll(context.Background(), authed(&syncrv1.PollRequest{Epoch: "e1", Wait: durationpb.New(time.Hour)}, id, secret))
		if err != nil || len(resp.Msg.GetCommands()) != 1 {
			t.Errorf("poll = %v, %v", resp, err)
		}
		done <- time.Now()
	}()
	time.Sleep(100 * time.Millisecond)
	queued := time.Now()
	e.queue(id)
	if latency := (<-done).Sub(queued); latency > 300*time.Millisecond {
		t.Errorf("woken after %v", latency)
	}
}

// TestPollEndsWithLifetime: a pending Poll is answered (empty) as soon as
// the server's lifetime ends.
func TestPollEndsWithLifetime(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{maxWait: 30 * time.Second})
	id, secret := e.enroll()
	go func() {
		time.Sleep(100 * time.Millisecond)
		e.stop()
	}()
	start := time.Now()
	resp, err := e.client().Poll(context.Background(), authed(&syncrv1.PollRequest{Epoch: "e1"}, id, secret))
	if err != nil || len(resp.Msg.GetCommands()) != 0 || time.Since(start) > 5*time.Second {
		t.Errorf("poll = %v, %v after %v", resp, err, time.Since(start))
	}
}

// TestPollOutlivesServerTimeouts: server-wide read and write timeouts
// shorter than a long poll cut neither the poll nor, afterwards, the next
// request on the same connection.
func TestPollOutlivesServerTimeouts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		read, write time.Duration
		pauseAfter  time.Duration
	}{
		{name: "short timeouts", read: 200 * time.Millisecond, write: 200 * time.Millisecond},
		// No server timeouts: the extended deadline must not stay on the
		// connection after the poll.
		{name: "no timeouts", pauseAfter: 1200 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, envOptions{
				maxWait: 800 * time.Millisecond, readTimeout: tt.read, writeTimeout: tt.write,
				link: active.Options{PollGrace: 100 * time.Millisecond},
			})
			id, secret := e.enroll()
			cl := e.client()
			go func() {
				time.Sleep(500 * time.Millisecond)
				e.queue(id)
			}()
			resp, err := cl.Poll(context.Background(), authed(&syncrv1.PollRequest{Epoch: "e1"}, id, secret))
			if err != nil || len(resp.Msg.GetCommands()) != 1 {
				t.Fatalf("poll = %v, %v", resp, err)
			}
			resp, err = cl.Poll(context.Background(), authed(&syncrv1.PollRequest{Epoch: "e1", Acks: []*syncrv1.CommandAck{{CommandId: resp.Msg.GetCommands()[0].GetId()}}}, id, secret))
			if err != nil || len(resp.Msg.GetCommands()) != 0 {
				t.Fatalf("empty poll = %v, %v", resp, err)
			}
			time.Sleep(tt.pauseAfter)
			if _, err := cl.Report(context.Background(), authed(&syncrv1.ReportRequest{Epoch: "e1"}, id, secret)); err != nil {
				t.Errorf("report on the reused connection: %v", err)
			}
		})
	}
}

func TestReadLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{link: active.Options{ReadMaxBytes: 64 << 10}})
	id, secret := e.enroll()
	big := &syncrv1.ReportRequest{Epoch: "e1", Events: []*syncrv1.Event{{Seq: 1, Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{Hostname: strings.Repeat("h", 100<<10)}}}}}
	if _, err := e.client().Report(context.Background(), authed(big, id, secret)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("oversized report: %v", err)
	}
}

func TestEnrollRateLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{link: active.Options{
		EnrollLimiter: active.NewRateLimiter(active.RateLimit{Every: time.Hour, Burst: 2}, nil),
	}})
	cl := syncrv1connect.NewEnrollmentServiceClient(e.srv.Client(), e.srv.URL)
	for i, want := range []connect.Code{connect.CodeUnauthenticated, connect.CodeUnauthenticated, connect.CodeResourceExhausted} {
		_, err := cl.Enroll(context.Background(), connect.NewRequest(&syncrv1.EnrollRequest{EnrollmentToken: "syncr_et_x"}))
		if connect.CodeOf(err) != want {
			t.Errorf("attempt %d: %v, want %v", i, err, want)
		}
	}
}

// stubDispatcher fails every call with a store error.
type stubDispatcher struct{ err error }

func (s stubDispatcher) Report(context.Context, domain.WorkerID, *syncrv1.ReportRequest) (*syncrv1.ReportResponse, error) {
	return nil, s.err
}

func (s stubDispatcher) Poll(context.Context, domain.WorkerID, *syncrv1.PollRequest) (*syncrv1.PollResponse, error) {
	return nil, s.err
}

func (s stubDispatcher) CompleteProbe(context.Context, domain.WorkerID, string, *syncrv1.ProbeResult) error {
	return s.err
}

func (stubDispatcher) MaxWait() time.Duration { return time.Second }

type stubWorkers struct{}

func (stubWorkers) Authenticate(context.Context, domain.WorkerID, string) (workers.Worker, error) {
	return workers.Worker{}, nil
}

func (stubWorkers) Enroll(context.Context, workers.EnrollParams) (workers.Enrollment, error) {
	return workers.Enrollment{}, errors.New("db at 10.0.0.5 is down")
}

// TestErrorMapping: domain errors keep their code; internal errors reveal
// nothing.
func TestErrorMapping(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err  error
		code connect.Code
		msg  string
	}{
		{errors.New("pq: password authentication failed for user syncr"), connect.CodeInternal, "internal error"},
		{errors.Join(dispatch.ErrStaleEpoch, domain.ErrFailedPrecondition), connect.CodeFailedPrecondition, ""},
		{domain.ErrConflict, connect.CodeAborted, ""},
		{domain.ErrNotFound, connect.CodeNotFound, ""},
		{domain.ErrUnauthenticated, connect.CodeUnauthenticated, "invalid worker credentials"},
	} {
		h, err := active.New(active.Options{Dispatcher: stubDispatcher{tt.err}, Workers: stubWorkers{}, Lifetime: context.Background()})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		cl := syncrv1connect.NewControllerLinkServiceClient(srv.Client(), srv.URL)
		_, err = cl.Report(context.Background(), authed(&syncrv1.ReportRequest{Epoch: "e"}, domain.NewWorkerID(), "s"))
		var ce *connect.Error
		if !errors.As(err, &ce) || ce.Code() != tt.code || (tt.msg != "" && ce.Message() != tt.msg) {
			t.Errorf("%v: got %v", tt.err, err)
		}
		if tt.code == connect.CodeInternal {
			ecl := syncrv1connect.NewEnrollmentServiceClient(srv.Client(), srv.URL)
			_, err := ecl.Enroll(context.Background(), connect.NewRequest(&syncrv1.EnrollRequest{}))
			if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal || strings.Contains(err.Error(), "10.0.0.5") {
				t.Errorf("enroll internal error: %v", err)
			}
		}
		srv.Close()
	}
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	if _, err := active.New(active.Options{Workers: stubWorkers{}, Lifetime: context.Background()}); err == nil {
		t.Error("missing dispatcher accepted")
	}
	if _, err := active.New(active.Options{Dispatcher: stubDispatcher{}, Workers: stubWorkers{}}); err == nil {
		t.Error("missing lifetime accepted")
	}
	if _, err := active.New(active.Options{Dispatcher: stubDispatcher{}, Workers: stubWorkers{}, Lifetime: context.Background(), ReadMaxBytes: -1}); err == nil {
		t.Error("negative limit accepted")
	}
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := active.NewRateLimiter(active.RateLimit{Every: time.Second, Burst: 2, MaxKeys: 2}, c)
	for i, want := range []bool{true, true, false} {
		if got := l.Allow("a"); got != want {
			t.Fatalf("attempt %d: Allow = %v; the burst of 2 is not enforced", i, got)
		}
	}
	c.Advance(time.Second)
	if first, second := l.Allow("a"), l.Allow("a"); !first || second {
		t.Error("refill of one token per second not applied")
	}
	// b fills the second slot; new keys then share the overflow bucket.
	if !l.Allow("b") {
		t.Fatal("b denied")
	}
	if c, d, e := l.Allow("c"), l.Allow("d"), l.Allow("e"); !c || !d || e {
		t.Error("overflow bucket not shared by new keys")
	}
	// Once a's and b's buckets are full again they are forgotten.
	c.Advance(time.Minute)
	if !l.Allow("x") || !l.Allow("y") {
		t.Error("idle keys not pruned")
	}
}

func TestPeerIP(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"10.1.2.3:4567": "10.1.2.3", "[::1]:80": "::1", "weird": "weird"} {
		if got := active.PeerIP(connect.Peer{Addr: in}, nil); got != want {
			t.Errorf("PeerIP(%q) = %q", in, got)
		}
	}
}
