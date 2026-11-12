package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/outbox"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

func TestNewValidates(t *testing.T) {
	t.Parallel()
	reg := runner.NewRegistry()
	for _, cfg := range []Config{
		{},
		{Runners: reg, MaxConcurrentJobs: -1},
		{Runners: reg, ProgressInterval: -time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestHelloIsFirstEvent(t *testing.T) {
	t.Parallel()
	reg := runner.NewRegistry()
	if err := reg.Register(newFakeRunner(succeed)); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		Runners: reg,
		Identity: Identity{
			WorkerVersion: "v2.0.0-test", OS: "linux", Arch: "arm64", Hostname: "nas",
			Tools:      []Tool{{Name: "rclone", Version: "v1.75.2"}, {Name: "restic", Version: "0.19.1"}},
			LocalRoots: []string{"/srv"},
			Features:   []string{"probe", "restic.v2"},
			ProbeOps:   []string{"list_local", "restic_ls"},
		},
		MaxConcurrentJobs: 3,
		Outbox:            outbox.Options{Epoch: "epoch-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, a)
	evs, _ := a.Outbox().Pending(0, 0)
	if len(evs) != 1 || evs[0].GetSeq() != 1 {
		t.Fatalf("events = %v", evs)
	}
	h := evs[0].GetHello()
	if h.GetWorkerVersion() != "v2.0.0-test" || h.GetOs() != "linux" || h.GetArch() != "arm64" || h.GetHostname() != "nas" ||
		h.GetMaxConcurrentJobs() != 3 || !slices.Equal(h.GetJobKinds(), []string{"rclone_transfer"}) ||
		!slices.Equal(h.GetLocalRoots(), []string{"/srv"}) || len(h.GetTools()) != 2 ||
		h.GetTools()[1].GetName() != "restic" || h.GetTools()[1].GetVersion() != "0.19.1" ||
		!slices.Equal(h.GetFeatures(), []string{"probe", "restic.v2"}) || !slices.Equal(h.GetProbeOps(), []string{"list_local", "restic_ls"}) {
		t.Errorf("hello = %v", h)
	}
	if a.Epoch() != "epoch-1" {
		t.Errorf("Epoch() = %q", a.Epoch())
	}
}

func TestJobSucceeds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		output := &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: "abc"}}}
		fr := newFakeRunner(func(_ context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
			if job.ID != "a" || job.Spec.GetRcloneTransfer() == nil {
				t.Errorf("job = %+v", job)
			}
			emit(runner.Progress{Bytes: 5, TotalBytes: 10})
			time.Sleep(3 * time.Second)
			emit(runner.Progress{Bytes: 10, TotalBytes: 10})
			return runner.Result{
				ExitCode: 0,
				Stats:    runner.Progress{Bytes: 10, TotalBytes: 10, Files: 2, Elapsed: 3 * time.Second},
				Output:   output,
				Warnings: []string{"", "one", strings.Repeat("x", 600)},
			}, nil
		})
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		t0 := time.Now()
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait()
		if got := stateOf(a.State()); got != "a(running)" {
			t.Errorf("state while running = %q", got)
		}
		time.Sleep(5 * time.Second)

		if got, want := describe(events(a)), "start(a) progress(a,5) progress(a,10) result(a,SUCCEEDED)"; got != want {
			t.Fatalf("events:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "a")
		if r.GetError() != nil || r.GetExitCode() != 0 || r.GetStats().GetFiles() != 2 || r.GetStats().GetElapsed().AsDuration() != 3*time.Second {
			t.Errorf("result = %v", r)
		}
		if r.GetOutput().GetResticBackup().GetSnapshotId() != "abc" {
			t.Errorf("output = %v", r.GetOutput())
		}
		if w := r.GetWarnings(); len(w) != 2 || w[0] != "one" || len(w[1]) != runner.MaxWarningBytes+3 {
			t.Errorf("warnings = %q (limited by runner.LimitWarnings)", w)
		}
		if !r.GetStartedAt().AsTime().Equal(t0) || !r.GetFinishedAt().AsTime().Equal(t0.Add(3*time.Second)) {
			t.Errorf("started %v finished %v, want %v and +3s", r.GetStartedAt().AsTime(), r.GetFinishedAt().AsTime(), t0)
		}
		if got := stateOf(a.State()); got != "" {
			t.Errorf("state after finish = %q", got)
		}
	})
}

func TestJobFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		code syncrv1.JobErrorCode
	}{
		{"coded", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "source %q not found", "/x"), syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND},
		{"plain", errors.New("boom"), syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fr := newFakeRunner(func(_ context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
					emit(runner.Progress{Bytes: 3})
					return runner.Result{ExitCode: 7, Stats: runner.Progress{Bytes: 3}, Warnings: []string{"dropped"}}, tt.err
				})
				a := newAgent(t, fr, Config{})
				defer shutdown(t, a)
				handle(t, a, startCmd("c1", "a"))
				synctest.Wait()
				if got := describe(events(a)); got != "start(a) progress(a,3) result(a,FAILED)" {
					t.Fatalf("events = %s", got)
				}
				r := resultOf(t, a, "a")
				if r.GetError().GetCode() != tt.code || r.GetError().GetMessage() != tt.err.Error() ||
					r.GetExitCode() != 7 || r.GetStats().GetBytes() != 3 || len(r.GetWarnings()) != 0 || r.GetStartedAt() == nil {
					t.Errorf("result = %v", r)
				}
			})
		})
	}
}

func TestRejectedSpecs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		handle(t, a,
			&syncrv1.Command{Id: "c1", Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: "nospec"}}},
			&syncrv1.Command{Id: "c2", Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: "backup", Spec: &syncrv1.JobSpec{
				Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{}},
			}}}},
		)
		if got := describe(events(a)); got != "result(nospec,FAILED) result(backup,FAILED)" {
			t.Fatalf("events = %s", got)
		}
		if c := resultOf(t, a, "nospec").GetError().GetCode(); c != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
			t.Errorf("nospec code = %v", c)
		}
		r := resultOf(t, a, "backup")
		if r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND || r.GetStartedAt() != nil || r.GetExitCode() != -1 {
			t.Errorf("backup result = %v", r)
		}
		// Rejected jobs are known: a redelivered StartJob does not report again.
		handle(t, a, startCmd("c3", "nospec"))
		if got := len(events(a)); got != 2 {
			t.Errorf("%d events after duplicate", got)
		}
	})
}

func TestMalformedCommands(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		acks := a.HandleCommands(t.Context(), []*syncrv1.Command{
			{Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: "noid", Spec: transferSpec()}}}, // no id: not acked, ignored
			{Id: "unknown"}, // no command: acked, ignored
			{Id: "nojob", Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{Spec: transferSpec()}}},
			{Id: "nocancel", Command: &syncrv1.Command_CancelJob{CancelJob: &syncrv1.CancelJob{}}},
		})
		var ids []string
		for _, ack := range acks {
			ids = append(ids, ack.GetCommandId())
		}
		if !slices.Equal(ids, []string{"unknown", "nojob", "nocancel"}) {
			t.Errorf("acks = %v", ids)
		}
		synctest.Wait()
		if got := describe(events(a)); got != "" {
			t.Errorf("events = %s", got)
		}
		if fr.runCount("noid") != 0 || fr.runCount("") != 0 {
			t.Error("malformed start ran")
		}
	})
}

// TestStartJobIsIdempotent redelivers StartJob in every state of a job.
func TestStartJobIsIdempotent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
		fr := newFakeRunner(nil)
		fr.set("a", gate(release["a"], 0))
		fr.set("b", gate(release["b"], 0))
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 1})
		defer shutdown(t, a)

		handle(t, a, startCmd("c1", "a"), startCmd("c2", "b"))
		synctest.Wait()
		if got := stateOf(a.State()); got != "a(running) b(queued)" {
			t.Fatalf("state = %q", got)
		}
		// Running (a) and queued (b) duplicates, also within one batch.
		handle(t, a, startCmd("c1", "a"), startCmd("c3", "a"), startCmd("c2", "b"), startCmd("c4", "b"))
		synctest.Wait()
		if got := stateOf(a.State()); got != "a(running) b(queued)" {
			t.Fatalf("state after duplicates = %q", got)
		}

		close(release["a"])
		synctest.Wait()
		// Finished with its result unacked (a), running (b).
		handle(t, a, startCmd("c1", "a"), startCmd("c2", "b"))
		close(release["b"])
		synctest.Wait()
		if got, want := describe(events(a)), "start(a) progress(a,10) result(a,SUCCEEDED) start(b) progress(b,10) result(b,SUCCEEDED)"; got != want {
			t.Fatalf("events:\n got %s\nwant %s", got, want)
		}
		// Finished with its result acked.
		if err := a.AckEvents(a.Outbox().Stats().Head); err != nil {
			t.Fatal(err)
		}
		handle(t, a, startCmd("c1", "a"), startCmd("c2", "b"))
		synctest.Wait()
		if got := describe(events(a)); got != "" {
			t.Errorf("events after acked duplicates = %s", got)
		}
		if fr.runCount("a") != 1 || fr.runCount("b") != 1 {
			t.Errorf("runs: a %d, b %d; want 1 each", fr.runCount("a"), fr.runCount("b"))
		}
	})
}

// TestFinishedJobsAreForgotten: the agent remembers a finished job (to
// ignore a redelivered StartJob) while its result is unacked and for
// finishedRetention after the ack, then forgets it, so the set stays
// bounded over the process lifetime.
func TestFinishedJobsAreForgotten(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		known := func(id string) bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.knownLocked(id)
		}
		size := func() (int, int) {
			a.mu.Lock()
			defer a.mu.Unlock()
			return len(a.finished), len(a.retiring)
		}

		handle(t, a, startCmd("c1", "a"))
		synctest.Wait()
		aSeq := resultSeq(t, a, "a")
		handle(t, a, startCmd("c2", "b"))
		synctest.Wait()

		// Unacked results are kept however long it takes.
		time.Sleep(2 * finishedRetention)
		if err := a.AckEvents(aSeq - 1); err != nil {
			t.Fatal(err)
		}
		if !known("a") || !known("b") {
			t.Fatal("a finished job with an unacked result was forgotten")
		}
		// a's result is acked: kept for the retention, b's still unacked.
		if err := a.AckEvents(aSeq); err != nil {
			t.Fatal(err)
		}
		time.Sleep(finishedRetention - time.Second)
		handle(t, a, startCmd("c1", "a")) // a late redelivery
		synctest.Wait()
		if !known("a") || fr.runCount("a") != 1 {
			t.Fatalf("within the retention: known %v, runs %d", known("a"), fr.runCount("a"))
		}
		time.Sleep(time.Second)
		if err := a.AckEvents(aSeq); err != nil { // a heartbeat's ack, nothing new
			t.Fatal(err)
		}
		if known("a") || !known("b") {
			t.Errorf("after the retention: a known %v, b known %v; want only b", known("a"), known("b"))
		}
		if f, r := size(); f != 1 || r != 1 {
			t.Errorf("finished %d, retiring %d; want 1 and 1", f, r)
		}

		// Many jobs later, only the recent ones are remembered.
		for i := range 500 {
			handle(t, a, startCmd(fmt.Sprintf("m%d", i), fmt.Sprintf("m%d", i)))
			synctest.Wait()
			if err := a.AckEvents(a.Outbox().Stats().Head); err != nil {
				t.Fatal(err)
			}
		}
		if f, _ := size(); f != 501 {
			t.Errorf("finished %d within the retention, want 501", f)
		}
		time.Sleep(finishedRetention)
		if err := a.AckEvents(a.Outbox().Stats().Head); err != nil {
			t.Fatal(err)
		}
		if f, r := size(); f != 0 || r != 0 {
			t.Errorf("finished %d, retiring %d after the retention; want none", f, r)
		}
	})
}

// resultSeq returns the seq of job's JobResult among the unacked events.
func resultSeq(t *testing.T, a *Agent, job string) int64 {
	t.Helper()
	for _, e := range events(a) {
		if e.GetJobResult().GetJobId() == job {
			return e.GetSeq()
		}
	}
	t.Fatalf("no result for %s", job)
	return 0
}

// TestCancelJob cancels jobs in every state.
func TestCancelJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fr := newFakeRunner(gate(release, 2*time.Second))
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 1})
		defer shutdown(t, a)

		// Unknown job: acked, nothing happens.
		handle(t, a, cancelCmd("x1", "ghost"))
		synctest.Wait()
		if got := describe(events(a)); got != "" {
			t.Fatalf("cancel of unknown job produced %s", got)
		}

		handle(t, a, startCmd("c1", "run"), startCmd("c2", "queued"), startCmd("c3", "next"))
		synctest.Wait()

		// Queued: reported at once, without JobStarted, and never run.
		handle(t, a, cancelCmd("x2", "queued"), cancelCmd("x3", "queued"))
		if got := stateOf(a.State()); got != "run(running) next(queued)" {
			t.Fatalf("state = %q", got)
		}
		r := resultOf(t, a, "queued")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED || r.GetStartedAt() != nil ||
			r.GetError().GetMessage() != ErrCancelled.Error() || r.GetExitCode() != -1 ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED {
			t.Errorf("queued result = %v", r)
		}

		// Running: the runner is cancelled; duplicates while it stops change
		// nothing. The freed slot goes to the next queued job.
		t0 := time.Now()
		handle(t, a, cancelCmd("x4", "run"))
		handle(t, a, cancelCmd("x4", "run"), cancelCmd("x5", "run"))
		time.Sleep(time.Minute)
		r = resultOf(t, a, "run")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED || r.GetError().GetMessage() != ErrCancelled.Error() ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED || r.GetStartedAt() == nil || !r.GetFinishedAt().AsTime().Equal(t0.Add(2*time.Second)) || r.GetStats().GetBytes() != 10 {
			t.Errorf("running result = %v", r)
		}
		if got := stateOf(a.State()); got != "next(running)" {
			t.Errorf("state = %q", got)
		}

		// Finished: acked, nothing happens.
		handle(t, a, cancelCmd("x6", "run"), cancelCmd("x7", "queued"))
		close(release)
		synctest.Wait()
		if got, want := describe(events(a)),
			"start(run) progress(run,10) result(queued,CANCELLED) result(run,CANCELLED) start(next) progress(next,10) result(next,SUCCEEDED)"; got != want {
			t.Fatalf("events:\n got %s\nwant %s", got, want)
		}
		if fr.runCount("queued") != 0 || fr.runCount("run") != 1 {
			t.Errorf("runs: queued %d, run %d", fr.runCount("queued"), fr.runCount("run"))
		}
	})
}

func TestRunnerSucceedingDespiteCancelIsSucceeded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(func(ctx context.Context, _ runner.Job, _ runner.EmitFunc) (runner.Result, error) {
			<-ctx.Done()
			return runner.Result{}, nil // finished anyway
		})
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait()
		handle(t, a, cancelCmd("x1", "a"))
		synctest.Wait()
		if s := resultOf(t, a, "a").GetStatus(); s != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
			t.Errorf("status = %v", s)
		}
	})
}

func TestConcurrencyLimitAndQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := map[string]chan struct{}{}
		fr := newFakeRunner(nil)
		jobs := []string{"j1", "j2", "j3", "j4", "j5"}
		for _, j := range jobs {
			release[j] = make(chan struct{})
			fr.set(j, gate(release[j], 0))
		}
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 2})
		defer shutdown(t, a)
		for i, j := range jobs {
			handle(t, a, startCmd("c"+j, j))
			if i == 1 {
				synctest.Wait()
			}
		}
		synctest.Wait()
		if got := stateOf(a.State()); got != "j1(running) j2(running) j3(queued) j4(queued) j5(queued)" {
			t.Fatalf("state = %q", got)
		}
		// Finishing j2 frees a slot for j3 (FIFO), not j4 or j5.
		close(release["j2"])
		synctest.Wait()
		if got := stateOf(a.State()); got != "j1(running) j3(running) j4(queued) j5(queued)" {
			t.Fatalf("state = %q", got)
		}
		close(release["j1"])
		close(release["j3"])
		synctest.Wait()
		close(release["j4"])
		close(release["j5"])
		synctest.Wait()

		var startOrder []string
		running, maxRunning := 0, 0
		for _, e := range events(a) {
			if s := e.GetJobStarted(); s != nil {
				startOrder = append(startOrder, s.GetJobId())
				running++
				maxRunning = max(maxRunning, running)
			}
			if e.GetJobResult() != nil {
				running--
			}
		}
		if !slices.Equal(startOrder, jobs) || maxRunning != 2 {
			t.Errorf("start order %v (max running %d), want %v with at most 2", startOrder, maxRunning, jobs)
		}
	})
}

func TestProgressThrottle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Samples every 300ms (bytes = sample index), 8 samples, then return
		// 100ms after the last: t = 0, .3, .6, .9, 1.2, 1.5, 1.8, 2.1; end 2.2.
		fr := newFakeRunner(func(_ context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
			for i := range 8 {
				if i > 0 {
					time.Sleep(300 * time.Millisecond)
				}
				emit(runner.Progress{Bytes: int64(i)})
			}
			time.Sleep(100 * time.Millisecond)
			return runner.Result{Stats: runner.Progress{Bytes: 7}}, nil
		})
		a := newAgent(t, fr, Config{ProgressInterval: time.Second})
		defer shutdown(t, a)
		t0 := time.Now()
		handle(t, a, startCmd("c1", "a"))
		time.Sleep(10 * time.Second)

		// Published: sample 0 at once; the newest kept sample when the
		// interval has passed (sample 3 at 1.0s, sample 6 at 2.0s); the final
		// sample 7 before the result even though the interval has not passed.
		var got []string
		for _, e := range events(a) {
			if p := e.GetProgress(); p != nil {
				got = append(got, strings.Join([]string{
					e.GetTime().AsTime().Sub(t0).String(), "=", time.Duration(p.GetStats().GetBytes()).String(),
				}, ""))
			}
		}
		want := []string{"0s=0s", "1s=3ns", "2s=6ns", "2.2s=7ns"}
		if !slices.Equal(got, want) {
			t.Errorf("progress (time=bytes) = %v, want %v", got, want)
		}
		if got := describe(events(a)); !strings.HasSuffix(got, "progress(a,7) result(a,SUCCEEDED)") {
			t.Errorf("events = %s", got)
		}
	})
}

func TestProgressThrottleTrailingEdge(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fr := newFakeRunner(func(_ context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
			emit(runner.Progress{Bytes: 1})
			time.Sleep(400 * time.Millisecond)
			emit(runner.Progress{Bytes: 2})
			<-release // a long quiet phase
			return runner.Result{}, nil
		})
		a := newAgent(t, fr, Config{ProgressInterval: time.Second})
		defer shutdown(t, a)
		t0 := time.Now()
		handle(t, a, startCmd("c1", "a"))
		time.Sleep(5 * time.Second)
		evs := events(a)
		if got := describe(evs); got != "start(a) progress(a,1) progress(a,2)" {
			t.Fatalf("events = %s", got)
		}
		if at := evs[2].GetTime().AsTime().Sub(t0); at != time.Second {
			t.Errorf("kept sample published after %v, want 1s", at)
		}
		close(release)
		synctest.Wait()
		// Nothing was pending, so no extra sample before the result.
		if got := describe(events(a)); got != "start(a) progress(a,1) progress(a,2) result(a,SUCCEEDED)" {
			t.Errorf("events = %s", got)
		}
	})
}

func TestRunnerPanicIsInternalFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(succeed)
		fr.set("bad", func(_ context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
			emit(runner.Progress{Bytes: 42})
			emit(runner.Progress{Bytes: 43}) // kept by the throttle
			panic("runner bug")
		})
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 1})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "bad"), startCmd("c2", "good"))
		synctest.Wait()
		if got, want := describe(events(a)), "start(bad) progress(bad,42) progress(bad,43) result(bad,FAILED) start(good) progress(good,1) result(good,SUCCEEDED)"; got != want {
			t.Fatalf("events:\n got %s\nwant %s", got, want)
		}
		r := resultOf(t, a, "bad")
		if r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL || r.GetExitCode() != -1 || r.GetStats().GetBytes() != 43 ||
			strings.Contains(r.GetError().GetMessage(), "runner bug") {
			t.Errorf("result = %v", r)
		}
	})
}

func TestShutdownCancelsJobs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		const grace = 3 * time.Second
		fr := newFakeRunner(gate(release, grace))
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 2})
		handle(t, a, startCmd("c1", "r1"), startCmd("c2", "r2"), startCmd("c3", "q1"))
		synctest.Wait()

		t0 := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if took := time.Since(t0); took != grace {
			t.Errorf("Shutdown took %v, want the runners' %v grace", took, grace)
		}
		if got := stateOf(a.State()); got != "" {
			t.Errorf("state after shutdown = %q", got)
		}
		for _, job := range []string{"r1", "r2", "q1"} {
			r := resultOf(t, a, job)
			if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED || r.GetError().GetMessage() != ErrShuttingDown.Error() ||
				r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_SHUTDOWN {
				t.Errorf("%s result = %v", job, r)
			}
		}
		if fr.runCount("q1") != 0 {
			t.Error("queued job ran during shutdown")
		}

		// The queued job's result says it never started.
		if r := resultOf(t, a, "q1"); r.GetStartedAt() != nil {
			t.Errorf("q1 result has a start time: %v", r)
		}

		// Later StartJobs are refused, new or known: no ack, no event, so
		// the controller keeps the jobs queued for the next process. Other
		// commands are still acked.
		before := a.Outbox().Stats().Head
		acks := a.HandleCommands(t.Context(), []*syncrv1.Command{
			startCmd("c4", "late"), startCmd("c1", "r1"), cancelCmd("c5", "r2"), startCmd("c3", "q1"),
		})
		if len(acks) != 1 || acks[0].GetCommandId() != "c5" {
			t.Errorf("acks after shutdown = %v, want only the CancelJob", acks)
		}
		if head := a.Outbox().Stats().Head; head != before {
			t.Errorf("refused commands appended %d events: %s", head-before, describe(events(a)))
		}
		if got := describe(events(a)); strings.Count(got, "result(r1") != 1 || strings.Contains(got, "late") {
			t.Errorf("events = %s", got)
		}
		if fr.runCount("late") != 0 {
			t.Error("a job started during shutdown")
		}
		if err := a.Shutdown(ctx); err != nil {
			t.Errorf("second Shutdown: %v", err)
		}
	})
}

func TestShutdownTimesOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fr := newFakeRunner(func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			<-release // ignores cancellation
			return runner.Result{}, nil
		})
		a := newAgent(t, fr, Config{})
		handle(t, a, startCmd("c1", "stuck"))
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if err := a.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown = %v, want deadline exceeded", err)
		}
		close(release)
		shutdown(t, a)
		if s := resultOf(t, a, "stuck").GetStatus(); s != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
			t.Errorf("status = %v", s)
		}
	})
}

// TestBackpressure checks that a saturated outbox delays job starts (not
// results), and that a job waiting for capacity can be cancelled.
func TestBackpressure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fr := newFakeRunner(gate(release, 0))
		// Hello + start(a) + progress(a) = 3 = high water.
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 3, Outbox: outbox.Options{CoalesceAt: 3, HighWater: 3}})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait()
		handle(t, a, startCmd("c2", "b"), startCmd("c3", "c"))
		synctest.Wait()
		if got := stateOf(a.State()); got != "a(running) b(queued) c(queued)" {
			t.Fatalf("state while saturated = %q", got)
		}
		// Results are still appended while saturated.
		handle(t, a, cancelCmd("x1", "c"))
		synctest.Wait()
		if got := describe(events(a)); got != "start(a) progress(a,10) result(c,CANCELLED)" {
			t.Fatalf("events = %s", got)
		}
		// Acks free capacity: b starts.
		if err := a.AckEvents(a.Outbox().Stats().Head); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := stateOf(a.State()); got != "a(running) b(running)" {
			t.Errorf("state after ack = %q", got)
		}
		close(release)
		synctest.Wait()
		if fr.runCount("c") != 0 {
			t.Error("cancelled job ran")
		}
	})
}

// TestSnapshotIsConsistent checks that the state in a snapshot matches the
// events in it, whatever the limit.
func TestSnapshotIsConsistent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fr := newFakeRunner(succeed)
		fr.set("long", gate(release, 0))
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 2, Outbox: outbox.Options{Epoch: "e"}})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "long"), startCmd("c2", "short"))
		synctest.Wait()
		// Outbox: 1 hello, then start/progress for both jobs and short's
		// result, in some interleaving.
		all := a.Snapshot(0, 0, 0)
		if all.Epoch != "e" || all.More || len(all.Events) != 6 {
			t.Fatalf("snapshot = %s more=%v", describe(all.Events), all.More)
		}
		for limit := 1; limit <= 6; limit++ {
			s := a.Snapshot(0, limit, 0)
			included := strings.Contains(describe(s.Events), "result(short")
			listed := strings.Contains(stateOf(s.State), "short(")
			if included == listed {
				t.Errorf("limit %d: result included %v, short listed %v; want exactly one", limit, included, listed)
			}
			if !strings.Contains(stateOf(s.State), "long(running)") {
				t.Errorf("limit %d: state %q lacks long", limit, stateOf(s.State))
			}
			if s.More != (limit < 6) {
				t.Errorf("limit %d: more = %v", limit, s.More)
			}
		}
		close(release)
	})
}

// TestSnapshotStateFields checks last_seq, outbox_backlog and
// outbox_saturated.
func TestSnapshotStateFields(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{Outbox: outbox.Options{CoalesceAt: 3, HighWater: 3}})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait() // hello, start, progress, result = seqs 1..4

		s := a.Snapshot(0, 2, 0)
		if st := s.State; st.GetLastSeq() != 2 || st.GetOutboxBacklog() != 4 || !st.GetOutboxSaturated() {
			t.Errorf("limited snapshot state = %v", st)
		}
		s = a.Snapshot(0, 0, 0)
		if st := s.State; st.GetLastSeq() != 4 || len(st.GetJobs()) != 0 {
			t.Errorf("full snapshot state = %v", st)
		}
		// No events after afterSeq: last_seq is the head.
		if err := a.AckEvents(4); err != nil {
			t.Fatal(err)
		}
		s = a.Snapshot(4, 0, 0)
		if st := s.State; len(s.Events) != 0 || st.GetLastSeq() != 4 || st.GetOutboxBacklog() != 0 || st.GetOutboxSaturated() {
			t.Errorf("empty snapshot state = %v", st)
		}
	})
}

// TestSnapshotByteBudget checks that a byte budget limits the events but
// keeps the state consistent and always returns one event.
func TestSnapshotByteBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			return runner.Result{Warnings: []string{strings.Repeat("w", 400)}}, nil
		})
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait() // hello, start(a), result(a)
		s := a.Snapshot(1, 0, 10)
		if describe(s.Events) != "start(a)" || !s.More || stateOf(s.State) != "a(running)" || s.State.GetLastSeq() != 2 {
			t.Errorf("snapshot = %s more=%v state=%q", describe(s.Events), s.More, stateOf(s.State))
		}
		s = a.Snapshot(2, 0, 10)
		if describe(s.Events) != "result(a,SUCCEEDED)" || s.More || stateOf(s.State) != "" {
			t.Errorf("snapshot = %s more=%v state=%q", describe(s.Events), s.More, stateOf(s.State))
		}
	})
}

func TestOutputIsBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		var snaps []*syncrv1.ResticSnapshot
		for i := range 1000 {
			snaps = append(snaps, &syncrv1.ResticSnapshot{
				Id: fmt.Sprintf("%064d", i), Time: timestamppb.New(base.Add(time.Duration(i) * time.Hour)),
				Host: "h", Paths: []string{"/data/some/path"},
			})
		}
		out := &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshotsOutput{Snapshots: snaps}}}
		fr := newFakeRunner(func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			return runner.Result{Output: out, Warnings: []string{"w1", "w2", "w3", "w4", "w5", "w6", "w7", "w8", "w9", "w10"}}, nil
		})
		const limit = 16 << 10
		a := newAgent(t, fr, Config{MaxOutputBytes: limit})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait()
		r := resultOf(t, a, "a")
		got := r.GetOutput().GetResticSnapshots().GetSnapshots()
		if size := proto.Size(r.GetOutput()); size > limit || len(got) == 0 || len(got) >= 1000 {
			t.Fatalf("output: %d bytes, %d snapshots", size, len(got))
		}
		// The newest ones, in the original order.
		if last := got[len(got)-1].GetId(); last != fmt.Sprintf("%064d", 999) {
			t.Errorf("last kept snapshot = %s", last)
		}
		if first := got[0].GetId(); first != fmt.Sprintf("%064d", 1000-len(got)) {
			t.Errorf("first kept snapshot = %s with %d kept", first, len(got))
		}
		if w := r.GetWarnings(); len(w) != runner.MaxWarnings || !strings.HasPrefix(w[0], "output truncated: listing the newest") {
			t.Errorf("warnings = %q", w)
		}
		if len(out.GetResticSnapshots().GetSnapshots()) != 1000 {
			t.Error("the runner's output was modified")
		}
	})
}

func TestLimitOutput(t *testing.T) {
	t.Parallel()
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("%064d", i)
	}
	forget := &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{RemovedSnapshotIds: ids}}}
	got, warning := limitOutput(forget, 1000)
	if n := len(got.GetResticForget().GetRemovedSnapshotIds()); n == 0 || n >= 100 || proto.Size(got) > 1000 || !strings.Contains(warning, "of 100 removed") {
		t.Errorf("forget: %d ids, %d bytes, warning %q", n, proto.Size(got), warning)
	}
	backup := &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{SnapshotId: strings.Repeat("a", 100)}}}
	if got, warning := limitOutput(backup, 50); got != nil || !strings.HasPrefix(warning, "output dropped") {
		t.Errorf("backup: %v %q", got, warning)
	}
	if got, warning := limitOutput(backup, 1000); got != backup || warning != "" {
		t.Errorf("small output changed: %v %q", got, warning)
	}
	if got, warning := limitOutput(nil, 0); got != nil || warning != "" {
		t.Errorf("nil output: %v %q", got, warning)
	}
}

func TestErrorMessagesAreBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			return runner.Result{ExitCode: 1}, errors.New(strings.Repeat("é", 10_000))
		})
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "a"))
		synctest.Wait()
		msg := resultOf(t, a, "a").GetError().GetMessage()
		if len(msg) > MaxErrorMessageBytes || !strings.HasSuffix(msg, "...") || !utf8.ValidString(msg) {
			t.Errorf("message of %d bytes, valid %v", len(msg), utf8.ValidString(msg))
		}
	})
}

// TestResultTextIsValidUTF8: tool output that is not UTF-8 (a remote's
// error body in another encoding, quoted on a line that is not JSON) may
// reach a job's error message or warnings. A proto string field must be
// valid UTF-8 to be encoded, so a result carrying such bytes could never
// be sent, and with it every event behind it in the outbox.
func TestResultTextIsValidUTF8(t *testing.T) {
	t.Parallel()
	const bad = "rclone: 403 Acc\xe8s refus\xe9"
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			return runner.Result{ExitCode: 1}, errors.New(bad)
		})
		fr.set("warned", func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			return runner.Result{Warnings: []string{bad, strings.Repeat("\xe9", 2*runner.MaxWarningBytes)}}, nil
		})
		fr.set("long", func(context.Context, runner.Job, runner.EmitFunc) (runner.Result, error) {
			return runner.Result{ExitCode: 1}, errors.New(strings.Repeat("\xe9", 10_000))
		})
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "failed"), startCmd("c2", "warned"), startCmd("c3", "long"))
		synctest.Wait()
		for _, ev := range events(a) {
			if _, err := proto.Marshal(ev); err != nil {
				t.Errorf("event %d cannot be encoded: %v", ev.GetSeq(), err)
			}
		}
		if got, want := resultOf(t, a, "failed").GetError().GetMessage(), "rclone: 403 Acc�s refus�"; got != want {
			t.Errorf("message = %q, want %q", got, want)
		}
		if got := resultOf(t, a, "warned").GetWarnings(); len(got) != 2 || got[0] != "rclone: 403 Acc�s refus�" ||
			len(got[1]) > runner.MaxWarningBytes+len("...") {
			t.Errorf("warnings = %q", got)
		}
		if msg := resultOf(t, a, "long").GetError().GetMessage(); len(msg) > MaxErrorMessageBytes || !utf8.ValidString(msg) {
			t.Errorf("message of %d bytes, valid %v", len(msg), utf8.ValidString(msg))
		}
	})
}
