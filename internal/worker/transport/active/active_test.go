package active

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/agent"
	"github.com/Zeptiny/Syncr/internal/worker/linkcore"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// inMemory is a RoundTripper that serves requests with a handler in the
// calling goroutine, so the whole link runs inside a synctest bubble.
type inMemory struct{ h http.Handler }

func (m inMemory) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, req)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	return rec.Result(), nil
}

// fakeController is a ControllerLinkService that acks everything it gets,
// hands out queued commands and can fail calls.
type fakeController struct {
	mu       sync.Mutex
	reports  []time.Time
	polls    []time.Time
	auth     []string
	events   map[int64]*syncrv1.Event
	acked    []string
	commands []*syncrv1.Command
	wake     chan struct{}
	failWith error // returned by every call while set
	ackLess  bool  // ack nothing (for the drain deadline test)
}

func newFakeController() *fakeController {
	return &fakeController{events: map[int64]*syncrv1.Event{}, wake: make(chan struct{}, 1)}
}

func (f *fakeController) handler() http.Handler {
	_, h := syncrv1connect.NewControllerLinkServiceHandler(f)
	return h
}

func (f *fakeController) Report(_ context.Context, req *connect.Request[syncrv1.ReportRequest]) (*connect.Response[syncrv1.ReportResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, time.Now())
	f.auth = append(f.auth, req.Header().Get("Authorization"))
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.ack(req.Msg.GetAcks())
	var last int64
	for _, ev := range req.Msg.GetEvents() {
		f.events[ev.GetSeq()] = ev
		last = ev.GetSeq()
	}
	if f.ackLess {
		last = 0
	}
	return connect.NewResponse(&syncrv1.ReportResponse{AckSeq: last}), nil
}

func (f *fakeController) ack(acks []*syncrv1.CommandAck) {
	for _, a := range acks {
		f.acked = append(f.acked, a.GetCommandId())
		f.commands = slices.DeleteFunc(f.commands, func(c *syncrv1.Command) bool { return c.GetId() == a.GetCommandId() })
	}
}

func (f *fakeController) Poll(ctx context.Context, req *connect.Request[syncrv1.PollRequest]) (*connect.Response[syncrv1.PollResponse], error) {
	f.mu.Lock()
	f.polls = append(f.polls, time.Now())
	f.auth = append(f.auth, req.Header().Get("Authorization"))
	if err := f.failWith; err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.ack(req.Msg.GetAcks())
	cmds := slices.Clone(f.commands)
	f.mu.Unlock()
	if len(cmds) == 0 {
		t := time.NewTimer(req.Msg.GetWait().AsDuration())
		defer t.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		case <-f.wake:
			f.mu.Lock()
			cmds = slices.Clone(f.commands)
			f.mu.Unlock()
		}
	}
	return connect.NewResponse(&syncrv1.PollResponse{Commands: cmds}), nil
}

func (f *fakeController) queue(cmd *syncrv1.Command) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fakeController) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = err
}

func (f *fakeController) calls() (reports, polls []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reports), slices.Clone(f.polls)
}

// stepRunner emits one sample per second for steps seconds, then succeeds;
// it stops when cancelled.
type stepRunner struct{ steps int }

func (stepRunner) Kind() string { return "rclone_transfer" }

func (r stepRunner) Run(ctx context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	var p runner.Progress
	for range r.steps {
		select {
		case <-ctx.Done():
			return runner.Result{ExitCode: -1, Stats: p}, context.Cause(ctx)
		case <-time.After(time.Second):
		}
		p.Bytes++
		emit(p)
	}
	return runner.Result{Stats: p}, nil
}

// setup builds an agent, core and transport against ctrl.
func setup(t *testing.T, ctrl *fakeController, rn runner.Runner, mod func(*Options)) (*agent.Agent, *linkcore.Core, *Transport) {
	t.Helper()
	reg := runner.NewRegistry()
	if err := reg.Register(rn); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{Runners: reg, ProgressInterval: 100 * time.Millisecond, MaxConcurrentJobs: 8})
	if err != nil {
		t.Fatal(err)
	}
	core := linkcore.New(a, a.Outbox(), nil)
	opts := Options{
		ControllerURL: "https://controller.test/prefix/", WorkerID: "w1", Secret: "s3cret",
		HTTPClient: &http.Client{Transport: inMemory{h: http.StripPrefix("/prefix", ctrl.handler())}},
	}
	if mod != nil {
		mod(&opts)
	}
	tr, err := New(core, opts)
	if err != nil {
		t.Fatal(err)
	}
	return a, core, tr
}

func run(t *testing.T, tr *Transport) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tr.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func shutdownAgent(t *testing.T, a *agent.Agent) {
	t.Helper()
	if err := a.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func startJob(id string) *syncrv1.Command {
	return &syncrv1.Command{Id: "start:" + id, Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{
		JobId: id, Spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}},
	}}}
}

// TestHeartbeatAndAuth: an idle worker reports at once (its Hello), then
// every heartbeat, and keeps a long poll open; every call is authenticated.
func TestHeartbeatAndAuth(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		a, core, tr := setup(t, ctrl, stepRunner{}, nil)
		defer shutdownAgent(t, a)
		stop := run(t, tr)
		time.Sleep(time.Minute)
		synctest.Wait()
		stop()

		reports, polls := ctrl.calls()
		if len(reports) != 5 { // 0, 15, 30, 45, 60 s
			t.Errorf("%d reports in a minute: %v", len(reports), reports)
		}
		if len(polls) != 3 { // 0, 25, 50 s
			t.Errorf("%d polls in a minute", len(polls))
		}
		if !core.Drained() || ctrl.events[1].GetHello() == nil {
			t.Error("hello not delivered")
		}
		ctrl.mu.Lock()
		defer ctrl.mu.Unlock()
		for _, h := range ctrl.auth {
			if h != link.ActiveAuthorization("w1", "s3cret") {
				t.Errorf("authorization %q", h)
			}
		}
	})
}

// TestCommandsEventsAndRateLimit: a command is delivered by the long poll
// and acked by the next poll; the job's events are reported at most about
// once per ReportInterval.
func TestCommandsEventsAndRateLimit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		a, core, tr := setup(t, ctrl, stepRunner{steps: 10}, nil)
		defer shutdownAgent(t, a)
		stop := run(t, tr)
		defer stop()
		time.Sleep(time.Second)
		ctrl.queue(startJob("j1"))
		synctest.Wait()
		ctrl.mu.Lock()
		acked := slices.Clone(ctrl.acked)
		ctrl.mu.Unlock()
		// Undelivered acks ride on both Reports and Polls until one succeeds,
		// so the controller may see one twice.
		if len(slices.Compact(acked)) != 1 || acked[0] != "start:j1" {
			t.Fatalf("acked = %v", acked)
		}

		time.Sleep(15 * time.Second)
		synctest.Wait()
		reports, _ := ctrl.calls()
		for i := 1; i < len(reports); i++ {
			if gap := reports[i].Sub(reports[i-1]); gap < DefaultReportInterval {
				t.Errorf("reports %d and %d only %v apart", i-1, i, gap)
			}
		}
		if len(reports) < 10 {
			t.Errorf("only %d reports for 10 progress events", len(reports))
		}
		if !core.Drained() {
			t.Error("events left after the job")
		}
		var result *syncrv1.JobResult
		for _, ev := range ctrl.events {
			if r := ev.GetJobResult(); r != nil {
				result = r
			}
		}
		if result.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED || result.GetStats().GetBytes() != 10 {
			t.Errorf("result = %v", result)
		}
	})
}

// TestBacklogIsSentInFullBatches: while full batches leave events behind,
// the next Report goes out at once.
func TestBacklogIsSentInFullBatches(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		a, core, tr := setup(t, ctrl, stepRunner{}, func(o *Options) { o.MaxEvents = 2 })
		defer shutdownAgent(t, a)
		var cmds []*syncrv1.Command
		for _, id := range []string{"a", "b", "c", "d"} {
			cmds = append(cmds, startJob(id))
		}
		a.HandleCommands(t.Context(), cmds)
		synctest.Wait() // hello + 4 starts + 4 results = 9 events
		start := time.Now()
		stop := run(t, tr)
		defer stop()
		synctest.Wait()
		if !core.Drained() || time.Since(start) != 0 {
			t.Errorf("drained %v after %v", core.Drained(), time.Since(start))
		}
		if reports, _ := ctrl.calls(); len(reports) != 5 {
			t.Errorf("%d reports for 9 events in batches of 2", len(reports))
		}
	})
}

// TestBackoff: transport errors back off exponentially with jitter, up to
// MaxBackoff; permanent errors wait MaxBackoff at once. Nothing spins.
func TestBackoff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
		max  int
	}{
		// Waits double from 1 s to 30 s, jittered to [d/2, d]: at most 14
		// calls fit into 2 minutes. Permanent errors wait 15-30 s at once.
		{name: "unavailable", err: connect.NewError(connect.CodeUnavailable, errors.New("down")), max: 14},
		{name: "stale epoch", err: connect.NewError(connect.CodeFailedPrecondition, errors.New("stale")), max: 9},
		{name: "unauthenticated", err: connect.NewError(connect.CodeUnauthenticated, errors.New("no")), max: 9},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctrl := newFakeController()
				ctrl.fail(tt.err)
				a, _, tr := setup(t, ctrl, stepRunner{}, nil)
				defer shutdownAgent(t, a)
				stop := run(t, tr)
				time.Sleep(2 * time.Minute)
				synctest.Wait()
				reports, polls := ctrl.calls()
				if len(reports) > tt.max || len(polls) > tt.max || len(reports) < 4 {
					t.Errorf("%d reports and %d polls in 2 minutes", len(reports), len(polls))
				}
				for i := 1; i < len(reports); i++ {
					if gap := reports[i].Sub(reports[i-1]); gap < DefaultMinBackoff/2 {
						t.Errorf("reports %d apart", gap)
					}
				}

				// Recovery: the next attempt succeeds and delivers the Hello.
				ctrl.fail(nil)
				time.Sleep(DefaultMaxBackoff)
				synctest.Wait()
				stop()
				if ctrl.events[1].GetHello() == nil {
					t.Error("hello not delivered after recovery")
				}
			})
		})
	}
}

// TestDrain: after the agent shut down, Drain delivers the CANCELLED
// results; without acks it gives up at the deadline.
func TestDrain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		a, core, tr := setup(t, ctrl, stepRunner{steps: 1000}, nil)
		stop := run(t, tr)
		ctrl.queue(startJob("long"))
		time.Sleep(5 * time.Second)
		synctest.Wait()
		stop()
		shutdownAgent(t, a)
		if core.Drained() {
			t.Fatal("the cancelled result should be pending")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := tr.Drain(ctx); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		var result *syncrv1.JobResult
		for _, ev := range ctrl.events {
			if r := ev.GetJobResult(); r != nil {
				result = r
			}
		}
		if result.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN {
			t.Errorf("result = %v", result)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		ctrl.ackLess = true
		a, _, tr := setup(t, ctrl, stepRunner{}, nil)
		shutdownAgent(t, a)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := tr.Drain(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 10*time.Second {
			t.Errorf("Drain = %v after %v", err, time.Since(start))
		}
	})
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	core := linkcore.New(nil, nil, nil)
	for name, o := range map[string]Options{
		"no url":        {WorkerID: "w", Secret: "s"},
		"ftp url":       {ControllerURL: "ftp://c", WorkerID: "w", Secret: "s"},
		"credentials":   {ControllerURL: "https://u:p@c", WorkerID: "w", Secret: "s"},
		"no secret":     {ControllerURL: "https://c", WorkerID: "w"},
		"both clients":  {ControllerURL: "https://c", WorkerID: "w", Secret: "s", HTTPClient: http.DefaultClient, RootCAs: x509.NewCertPool()},
		"negative wait": {ControllerURL: "https://c", WorkerID: "w", Secret: "s", PollWait: -1},
	} {
		if _, err := New(core, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(core, Options{ControllerURL: "http://c", WorkerID: "w", Secret: "s"}); err != nil {
		t.Errorf("http url: %v", err)
	}
	if _, err := New(nil, Options{ControllerURL: "https://c", WorkerID: "w", Secret: "s"}); err == nil || !strings.Contains(err.Error(), "core") {
		t.Errorf("nil core: %v", err)
	}
}

// TestNoSpinWithoutAcks: full batches that the controller does not ack are
// resent at the report interval, not in a tight loop.
func TestNoSpinWithoutAcks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctrl := newFakeController()
		ctrl.ackLess = true
		a, _, tr := setup(t, ctrl, stepRunner{}, func(o *Options) { o.MaxEvents = 1 })
		defer shutdownAgent(t, a)
		a.HandleCommands(t.Context(), []*syncrv1.Command{startJob("a"), startJob("b")})
		synctest.Wait()
		stop := run(t, tr)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		stop()
		if reports, _ := ctrl.calls(); len(reports) > 11 {
			t.Errorf("%d reports in 10 s without acks", len(reports))
		}
	})
}
