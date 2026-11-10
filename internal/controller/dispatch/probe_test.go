package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// probeSecret is in every test probe's spec: it must never be logged.
const probeSecret = "probe-password-sentinel"

func runProbe(id string) *syncrv1.RunProbe {
	return &syncrv1.RunProbe{
		ProbeId: id,
		Spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ResticConfig{ResticConfig: &syncrv1.ProbeResticConfig{
			Repository: &syncrv1.ResticRepository{Location: &syncrv1.Location{Path: "/data/repo"}, Password: probeSecret},
		}}},
		Timeout: durationpb.New(10 * time.Second),
	}
}

func okResult(id string) *syncrv1.ProbeResult {
	return &syncrv1.ProbeResult{
		ProbeId: id, Status: syncrv1.ProbeStatus_PROBE_STATUS_OK,
		Output: &syncrv1.ProbeResult_RepositoryConfig{RepositoryConfig: &syncrv1.ResticRepositoryConfig{Id: strings.Repeat("cd", 32), Version: 2}},
	}
}

type probeAnswer struct {
	res *syncrv1.ProbeResult
	err error
}

// goProbe starts a Probe call and returns the channel of its answer.
func goProbe(ctx context.Context, e *env, worker domain.WorkerID, p *syncrv1.RunProbe) <-chan probeAnswer {
	ch := make(chan probeAnswer, 1)
	go func() {
		res, err := e.probes.Probe(ctx, worker, p)
		ch <- probeAnswer{res, err}
	}()
	return ch
}

// probeCommands returns the IDs of the probe commands in cmds.
func probeCommands(cmds []*syncrv1.Command) []string {
	var out []string
	for _, c := range cmds {
		if c.GetRunProbe() != nil || c.GetCancelProbe() != nil {
			out = append(out, c.GetId())
		}
	}
	return out
}

func (w *fakeWorker) complete(r *syncrv1.ProbeResult) error {
	return w.disp().CompleteProbe(context.Background(), w.id, w.epoch, r)
}

// commands derives the worker's commands without handling them.
func (w *fakeWorker) commands() []*syncrv1.Command {
	w.t.Helper()
	cmds, err := w.disp().Commands(context.Background(), w.id, w.epoch)
	if err != nil {
		w.t.Fatal(err)
	}
	return cmds
}

func TestProbeActiveDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		w := e.newProbeWorker(e.enroll("one"))
		w.report()
		j := e.run(w.id)

		// The probe wakes the worker's long poll.
		polled := make(chan []*syncrv1.Command, 1)
		go func() {
			cmds, _ := e.disp.WaitCommands(t.Context(), w.id, w.epoch, 20*time.Second)
			polled <- cmds
		}()
		synctest.Wait()
		// (The queued job already ended the wait; start over without it.)
		w.handle(<-polled)
		w.report()
		go func() {
			cmds, _ := e.disp.WaitCommands(t.Context(), w.id, w.epoch, 20*time.Second)
			polled <- cmds
		}()
		synctest.Wait()
		answer := goProbe(t.Context(), e, w.id, runProbe("p1"))
		synctest.Wait()
		cmds := <-polled
		if got := probeCommands(cmds); !slices.Equal(got, []string{"probe:p1"}) {
			t.Fatalf("commands = %v, want the run_probe", got)
		}
		if p := cmds[0].GetRunProbe(); p.GetProbeId() != "p1" || p.GetSpec().GetResticConfig().GetRepository().GetPassword() != probeSecret ||
			p.GetTimeout().AsDuration() != 10*time.Second {
			t.Errorf("run_probe = %v", p.GetProbeId())
		}

		// Until it is acked the command is derived again; afterwards not.
		if got := probeCommands(w.commands()); !slices.Equal(got, []string{"probe:p1"}) {
			t.Fatalf("before the ack: %v", got)
		}
		w.handle(cmds)
		if got := probeCommands(w.poll()); len(got) != 0 {
			t.Fatalf("after the ack: %v", got)
		}
		if len(w.probes) != 1 {
			t.Fatalf("the worker received %d probes", len(w.probes))
		}

		// Results of other probes and other workers are ignored.
		other := e.newProbeWorker(e.enroll("two"))
		other.report()
		if err := other.complete(okResult("p1")); err != nil {
			t.Fatalf("another worker's result: %v", err)
		}
		if err := w.complete(okResult("unknown")); err != nil {
			t.Fatalf("an unknown result: %v", err)
		}
		synctest.Wait()
		select {
		case a := <-answer:
			t.Fatalf("the probe ended early: %v %v", a.res, a.err)
		default:
		}

		if err := w.complete(okResult("p1")); err != nil {
			t.Fatal(err)
		}
		a := <-answer
		if a.err != nil || a.res.GetRepositoryConfig().GetVersion() != 2 {
			t.Fatalf("probe = %v, %v", a.res, a.err)
		}
		// A late duplicate is fine; a result without an ID is not.
		if err := w.complete(okResult("p1")); err != nil {
			t.Errorf("duplicate result: %v", err)
		}
		if err := w.complete(&syncrv1.ProbeResult{}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("result without an ID: %v", err)
		}
		if got := e.job(j.ID).Status; got != domain.JobDispatched {
			t.Errorf("the job next to the probe = %s", got)
		}
		if strings.Contains(e.logs.String(), probeSecret) {
			t.Error("the probe's spec was logged")
		}
	})
}

func TestProbeCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		w := e.newProbeWorker(e.enroll("one"))
		w.report()

		// Given up before the worker took it: nothing is sent.
		ctx, cancel := context.WithCancel(t.Context())
		answer := goProbe(ctx, e, w.id, runProbe("early"))
		synctest.Wait()
		cancel()
		if a := <-answer; !errors.Is(a.err, context.Canceled) {
			t.Fatalf("probe = %v", a.err)
		}
		if got := probeCommands(w.poll()); len(got) != 0 {
			t.Fatalf("after an early cancel: %v", got)
		}

		// Given up after the worker acked it: the worker is told to stop
		// it, until it acks that.
		ctx, cancel = context.WithCancel(t.Context())
		answer = goProbe(ctx, e, w.id, runProbe("late"))
		synctest.Wait()
		w.poll()
		w.poll() // acks the run_probe
		cancel()
		if a := <-answer; !errors.Is(a.err, context.Canceled) {
			t.Fatalf("probe = %v", a.err)
		}
		if got := probeCommands(w.commands()); !slices.Equal(got, []string{"probe-cancel:late"}) {
			t.Fatalf("after a late cancel: %v", got)
		}
		w.poll()
		if got := probeCommands(w.poll()); len(got) != 0 || !slices.Equal(w.probeCancels, []string{"late"}) {
			t.Fatalf("after the cancel's ack: %v, cancels %v", got, w.probeCancels)
		}
		// Its late result is ignored.
		if err := w.complete(okResult("late")); err != nil {
			t.Error(err)
		}
	})
}

func TestProbeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.probeCfg = dispatch.ProbeConfig{MaxTimeout: 3 * time.Second, Grace: time.Second}
		e.restart()
		w := e.newProbeWorker(e.enroll("one"))
		w.report()

		start := time.Now()
		answer := goProbe(t.Context(), e, w.id, runProbe("slow")) // asks for 10 s: capped at 3 s
		synctest.Wait()
		cmds := w.poll()
		if d := cmds[0].GetRunProbe().GetTimeout().AsDuration(); d != 3*time.Second {
			t.Errorf("timeout sent = %v, want the cap", d)
		}
		w.poll()
		a := <-answer
		if !errors.Is(a.err, domain.ErrUnavailable) || !strings.Contains(a.err.Error(), `worker "one" did not answer in time`) {
			t.Fatalf("probe = %v", a.err)
		}
		if got := time.Since(start); got != 4*time.Second {
			t.Errorf("gave up after %v, want timeout + grace", got)
		}
		if got := probeCommands(w.commands()); !slices.Equal(got, []string{"probe-cancel:slow"}) {
			t.Errorf("after the timeout: %v", got)
		}
	})
}

func TestProbeWorkerRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		w := e.newProbeWorker(e.enroll("one"))
		w.report()

		accepted := goProbe(t.Context(), e, w.id, runProbe("accepted"))
		synctest.Wait()
		w.poll()
		w.poll() // acked
		waiting := goProbe(t.Context(), e, w.id, runProbe("waiting"))
		synctest.Wait()

		w.restart()
		w.report()
		a := <-accepted
		if !errors.Is(a.err, domain.ErrUnavailable) || !strings.Contains(a.err.Error(), "restarted") {
			t.Fatalf("the accepted probe = %v", a.err)
		}
		// The one the dead process never took goes to the new process.
		if got := probeCommands(w.poll()); !slices.Equal(got, []string{"probe:waiting"}) {
			t.Fatalf("after the restart: %v", got)
		}
		if err := w.complete(okResult("waiting")); err != nil {
			t.Fatal(err)
		}
		if a := <-waiting; a.err != nil {
			t.Fatalf("the waiting probe = %v", a.err)
		}
	})
}

func TestProbeStaleEpochResult(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newProbeWorker(e.enroll("one"))
	w.report()
	old := w.epoch
	w.restart()
	w.report()
	err := e.disp.CompleteProbe(context.Background(), w.id, old, okResult("x"))
	if !errors.Is(err, dispatch.ErrStaleEpoch) {
		t.Fatalf("a result of a replaced process: %v", err)
	}
}

func TestProbeInFlightCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.probeCfg = dispatch.ProbeConfig{MaxInFlightPerWorker: 2}
		e.restart()
		w := e.newProbeWorker(e.enroll("one"))
		w.report()

		first := goProbe(t.Context(), e, w.id, runProbe("a"))
		synctest.Wait()
		second := goProbe(t.Context(), e, w.id, runProbe("b"))
		synctest.Wait()
		_, err := e.probes.Probe(t.Context(), w.id, runProbe("c"))
		if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "too many requests") {
			t.Fatalf("third probe = %v", err)
		}
		if got := probeCommands(w.poll()); !slices.Equal(got, []string{"probe:a", "probe:b"}) {
			t.Fatalf("commands = %v", got)
		}
		for _, id := range []string{"a", "b"} {
			if err := w.complete(okResult(id)); err != nil {
				t.Fatal(err)
			}
		}
		if a, b := <-first, <-second; a.err != nil || b.err != nil {
			t.Fatalf("probes = %v, %v", a.err, b.err)
		}
		// There is room again.
		third := goProbe(t.Context(), e, w.id, runProbe("c"))
		synctest.Wait()
		w.poll()
		if err := w.complete(okResult("c")); err != nil {
			t.Fatal(err)
		}
		if a := <-third; a.err != nil {
			t.Fatal(a.err)
		}
	})
}

// TestCommandBudget: probe and job commands share Config.MaxCommands.
// Probe commands get up to half of the room and the queued jobs the rest;
// what one side leaves unused goes to the other, so neither starves.
func TestCommandBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.cfg.MaxCommands = 8
		e.probeCfg = dispatch.ProbeConfig{MaxInFlightPerWorker: 32}
		e.restart()
		w := e.newProbeWorker(e.enroll("one"))
		w.report()
		for range 5 {
			e.run(w.id)
		}
		for i := range 20 {
			goProbe(t.Context(), e, w.id, runProbe(fmt.Sprintf("p%02d", i)))
			synctest.Wait()
		}
		// Each poll acks the previous round's commands.
		for i, want := range []struct{ probes, starts int }{{4, 4}, {7, 1}, {8, 0}, {1, 0}} {
			cmds := w.poll()
			if got := probeCommands(cmds); len(got) != want.probes || len(started(cmds)) != want.starts || len(cmds) != want.probes+want.starts {
				t.Fatalf("round %d: %d commands, probes %v, %d StartJob; want %d probes and %d StartJob",
					i+1, len(cmds), got, len(started(cmds)), want.probes, want.starts)
			}
			if i == 0 && (cmds[0].GetId() != "probe:p00" || cmds[3].GetId() != "probe:p03" || cmds[4].GetStartJob() == nil) {
				t.Errorf("round 1 = %v, want the oldest probes first, then the oldest jobs", cmds)
			}
		}

		// Without jobs, at most 16 run_probe commands go out at once.
		e2 := newEnv(t)
		e2.probeCfg = dispatch.ProbeConfig{MaxInFlightPerWorker: 32}
		e2.restart()
		w2 := e2.newProbeWorker(e2.enroll("one"))
		w2.report()
		for i := range 20 {
			goProbe(t.Context(), e2, w2.id, runProbe(fmt.Sprintf("q%02d", i)))
			synctest.Wait()
		}
		if got := probeCommands(w2.commands()); len(got) != 16 || got[0] != "probe:q00" || got[15] != "probe:q15" {
			t.Fatalf("commands = %v, want the 16 oldest run_probes", got)
		}
	})
}

// TestProbesNeverCrowdOutJobCancels: however many probe commands are
// pending, a job's CancelJob goes out at once and the total stays within
// Config.MaxCommands.
func TestProbesNeverCrowdOutJobCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.cfg.MaxCommands = 8
		e.probeCfg = dispatch.ProbeConfig{MaxInFlightPerWorker: 32}
		e.restart()
		w := e.newProbeWorker(e.enroll("one"))
		w.report()
		running := e.run(w.id)
		w.poll()
		w.start(running.ID.String())
		w.report()

		// Ten probes the worker accepted and whose callers then gave up:
		// ten cancel_probe commands.
		ctx, cancel := context.WithCancel(t.Context())
		var gone []<-chan probeAnswer
		for i := range 10 {
			gone = append(gone, goProbe(ctx, e, w.id, runProbe(fmt.Sprintf("gone%02d", i))))
			synctest.Wait()
		}
		for range 3 {
			w.poll() // delivers the run_probes, then acks them
		}
		if len(w.probes) != 10 {
			t.Fatalf("the worker received %d probes, want 10", len(w.probes))
		}
		cancel()
		for _, ch := range gone {
			<-ch
		}
		// Six more probes waiting, three queued jobs and a cancel request.
		for i := range 6 {
			goProbe(t.Context(), e, w.id, runProbe(fmt.Sprintf("wait%02d", i)))
			synctest.Wait()
		}
		for range 3 {
			e.run(w.id)
		}
		if _, err := e.jobs.Cancel(t.Context(), testActor(), running.ID); err != nil {
			t.Fatal(err)
		}

		cmds := w.commands()
		var cancels []string
		for _, c := range cmds {
			if c.GetCancelJob() != nil {
				cancels = append(cancels, c.GetCancelJob().GetJobId())
			}
		}
		if len(cmds) != 8 || !slices.Equal(cancels, []string{running.ID.String()}) || len(started(cmds)) != 3 || len(probeCommands(cmds)) != 4 {
			t.Fatalf("%d commands: probes %v, cancels %v, %d StartJob; want 8 with the CancelJob, 3 StartJob and 4 probe commands",
				len(cmds), probeCommands(cmds), cancels, len(started(cmds)))
		}
	})
}

func TestProbeRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	pending := e.enroll("pending")
	old := e.newFakeWorker(e.enroll("old"))
	old.report()
	current := e.newProbeWorker(e.enroll("current"))
	current.report()
	removed := e.newProbeWorker(e.enroll("removed"))
	removed.report()
	if err := e.workers.Remove(ctx, removed.id); err != nil {
		t.Fatal(err)
	}
	noOp := runProbe("x")
	noOp.Spec = &syncrv1.ProbeSpec{}

	tests := []struct {
		name   string
		worker domain.WorkerID
		probe  *syncrv1.RunProbe
		want   error
		msg    string
	}{
		{"unknown worker", domain.NewWorkerID(), runProbe("x"), domain.ErrNotFound, ""},
		{"removed worker", removed.id, runProbe("x"), domain.ErrNotFound, ""},
		{"never connected", pending, runProbe("x"), domain.ErrUnavailable, `worker "pending" has never connected`},
		{"outdated worker", old.id, runProbe("x"), domain.ErrFailedPrecondition, `worker "old" (version v2.0.0-test) cannot run restic_config; upgrade it`},
		{"no probe id", current.id, runProbe(""), domain.ErrInvalidArgument, ""},
		{"no operation", current.id, noOp, domain.ErrInvalidArgument, ""},
	}
	for _, tt := range tests {
		_, err := e.probes.Probe(ctx, tt.worker, tt.probe)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: err = %v, want %v with %q", tt.name, err, tt.want, tt.msg)
		}
		if err != nil && strings.Contains(err.Error(), probeSecret) {
			t.Errorf("%s: the error leaks the spec: %v", tt.name, err)
		}
	}

	// Offline: out of contact for longer than the workers' OfflineAfter.
	e.clock.Advance(time.Hour)
	if _, err := e.probes.Probe(ctx, current.id, runProbe("x")); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "is offline") {
		t.Errorf("offline worker: %v", err)
	}
	// An old worker is never sent a run_probe, whatever is queued.
	if got := probeCommands(old.poll()); len(got) != 0 {
		t.Errorf("the old worker was sent %v", got)
	}
}

func TestProbePassive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.probeCfg = dispatch.ProbeConfig{MaxInFlightPerWorker: 1, MaxTimeout: 30 * time.Second, Grace: 2 * time.Second}
		e.restart()
		wid := registerPassive(t, e)

		// No loop: the link is not running.
		if _, err := e.probes.Probe(t.Context(), wid, runProbe("x")); !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("without a loop (never connected): %v", err)
		}

		c, stop := startLoopOf(t, e, wid, e.newProbeWorker(wid))
		synctest.Wait()

		// A worker that answers Unimplemented.
		_, err := e.probes.Probe(t.Context(), wid, runProbe("x"))
		if !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), "upgrade it") {
			t.Fatalf("unimplemented: %v", err)
		}

		// A direct call with the probe's deadline.
		var deadline time.Duration
		c.do(func(*fakeWorker) {
			c.probe = func(ctx context.Context, _ *syncrv1.ProbeRequest) (*syncrv1.ProbeResponse, error) {
				d, _ := ctx.Deadline()
				deadline = time.Until(d)
				return &syncrv1.ProbeResponse{Result: okResult("")}, nil
			}
		})
		res, err := e.probes.Probe(t.Context(), wid, runProbe("direct"))
		if err != nil || res.GetProbeId() != "direct" || res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Fatalf("probe = %v, %v", res, err)
		}
		if deadline != 12*time.Second {
			t.Errorf("deadline = %v, want the timeout plus grace", deadline)
		}
		if got := probeCommands(c.w.probes2commands()); len(got) != 0 {
			t.Errorf("a passive worker was sent probe commands: %v", got)
		}

		// A worker that hangs: the caller gets Unavailable after the
		// deadline, and a second probe meanwhile is refused (cap 1).
		c.do(func(*fakeWorker) {
			c.probe = func(ctx context.Context, _ *syncrv1.ProbeRequest) (*syncrv1.ProbeResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
		})
		hung := goProbe(t.Context(), e, wid, runProbe("hung"))
		synctest.Wait()
		if _, err := e.probes.Probe(t.Context(), wid, runProbe("more")); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "too many requests") {
			t.Fatalf("over the cap: %v", err)
		}
		if a := <-hung; !errors.Is(a.err, domain.ErrUnavailable) || !strings.Contains(a.err.Error(), "did not answer in time") {
			t.Fatalf("hung probe = %v", a.err)
		}

		// A transport failure: no address in the message.
		c.do(func(*fakeWorker) {
			c.probe = func(context.Context, *syncrv1.ProbeRequest) (*syncrv1.ProbeResponse, error) {
				return nil, errors.New("dial tcp 10.1.2.3:7443: connection refused")
			}
		})
		if _, err := e.probes.Probe(t.Context(), wid, runProbe("down")); !errors.Is(err, domain.ErrUnavailable) || strings.Contains(err.Error(), "10.1.2.3") {
			t.Fatalf("transport failure: %v", err)
		}

		// The loop ends: its client is gone.
		stop()
		if _, err := e.probes.Probe(t.Context(), wid, runProbe("late")); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "is not running") {
			t.Fatalf("after the loop ended: %v", err)
		}
		if strings.Contains(e.logs.String(), probeSecret) {
			t.Error("the probe's spec was logged")
		}
	})
}

// probes2commands is the run_probe commands the fake worker received.
func (w *fakeWorker) probes2commands() []*syncrv1.Command {
	var out []*syncrv1.Command
	for _, p := range w.probes {
		out = append(out, &syncrv1.Command{Id: "probe:" + p.GetProbeId(), Command: &syncrv1.Command_RunProbe{RunProbe: p}})
	}
	return out
}

func TestProbeBrokerConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := dispatch.NewProbeBroker(dispatch.ProbeBrokerDeps{}, dispatch.ProbeConfig{}); err == nil {
		t.Error("missing dependencies accepted")
	}
	deps := dispatch.ProbeBrokerDeps{Workers: e.workers, Hub: e.hub, Clock: e.clock}
	if _, err := dispatch.NewProbeBroker(deps, dispatch.ProbeConfig{MaxInFlightPerWorker: -1}); err == nil {
		t.Error("negative cap accepted")
	}
	b, err := dispatch.NewProbeBroker(deps, dispatch.ProbeConfig{})
	if err != nil || b.MaxTimeout() != 90*time.Second {
		t.Errorf("defaults: %v, %v", b, err)
	}
	if !errors.Is(dispatch.ErrProbeUnsupported, domain.ErrFailedPrecondition) || !slices.Contains(link.Features(), link.FeatureProbe) {
		t.Error("ErrProbeUnsupported must be a failed precondition")
	}
}
