package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// fakeTimers replaces Config.AfterFunc: a test fires a time limit when it
// chooses instead of waiting for it.
type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
	fired   bool
}

func (ft *fakeTimers) afterFunc(d time.Duration, f func()) func() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	tm := &fakeTimer{d: d, f: f}
	ft.timers = append(ft.timers, tm)
	return func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		active := !tm.stopped && !tm.fired
		tm.stopped = true
		return active
	}
}

// only returns the single timer armed so far.
func (ft *fakeTimers) only(t *testing.T) *fakeTimer {
	t.Helper()
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.timers) != 1 {
		t.Fatalf("%d timers armed, want 1", len(ft.timers))
	}
	return ft.timers[0]
}

func (ft *fakeTimers) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.timers)
}

// fire runs the timer's function as the clock would, unless it was stopped.
func (ft *fakeTimers) fire(tm *fakeTimer) {
	ft.mu.Lock()
	if tm.stopped || tm.fired {
		ft.mu.Unlock()
		return
	}
	tm.fired = true
	ft.mu.Unlock()
	go tm.f()
}

func (ft *fakeTimers) isStopped(tm *fakeTimer) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return tm.stopped
}

// limitedStartCmd starts job with the given time limit.
func limitedStartCmd(cmdID, jobID string, limit time.Duration) *syncrv1.Command {
	c := startCmd(cmdID, jobID)
	c.GetStartJob().GetSpec().MaxRuntime = durationpb.New(limit)
	return c
}

func TestTimeLimitStopsJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var timers fakeTimers
		causes := make(chan error, 1)
		fr := newFakeRunner(func(ctx context.Context, _ runner.Job, emit runner.EmitFunc) (runner.Result, error) {
			emit(runner.Progress{Bytes: 10})
			<-ctx.Done()
			causes <- context.Cause(ctx)
			time.Sleep(2 * time.Second) // the tool's graceful stop
			return runner.Result{ExitCode: 3, Stats: runner.Progress{Bytes: 10}}, canceled(ctx)
		})
		a := newAgent(t, fr, Config{AfterFunc: timers.afterFunc})
		defer shutdown(t, a)

		handle(t, a, limitedStartCmd("c1", "a", 6*time.Hour))
		synctest.Wait()
		tm := timers.only(t)
		if tm.d != 6*time.Hour {
			t.Errorf("limit armed for %v, want 6h", tm.d)
		}
		timers.fire(tm)
		time.Sleep(time.Minute)

		if cause := <-causes; !errors.Is(cause, ErrTimeLimit) {
			t.Errorf("runner's context cause = %v, want ErrTimeLimit", cause)
		}
		r := resultOf(t, a, "a")
		if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
			r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT ||
			r.GetError().GetMessage() != "stopped after 6h0m0s: the job's time limit was reached" {
			t.Errorf("result = %v", r)
		}
		// The work done so far is reported.
		if r.GetStats().GetBytes() != 10 || r.GetExitCode() != 3 || r.GetStartedAt() == nil {
			t.Errorf("result stats = %v, exit %d", r.GetStats(), r.GetExitCode())
		}
		if got, want := describe(events(a)), "start(a) progress(a,10) result(a,FAILED)"; got != want {
			t.Errorf("events:\n got %s\nwant %s", got, want)
		}
	})
}

// TestTimeLimitWithTheRealClock runs the default AfterFunc (time.AfterFunc)
// on synctest's fake clock: the limit counts from the job's start, not
// from when it was queued.
func TestTimeLimitWithTheRealClock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		fr := newFakeRunner(gate(make(chan struct{}), 0))
		fr.set("first", gate(release, 0))
		a := newAgent(t, fr, Config{MaxConcurrentJobs: 1})
		defer shutdown(t, a)

		handle(t, a, startCmd("c1", "first"), limitedStartCmd("c2", "limited", time.Hour))
		time.Sleep(3 * time.Hour) // "limited" waits in the queue meanwhile
		synctest.Wait()
		if got := stateOf(a.State()); got != "first(running) limited(queued)" {
			t.Fatalf("state = %q", got)
		}
		close(release)
		synctest.Wait()
		t0 := time.Now()

		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if got := stateOf(a.State()); got != "limited(running)" {
			t.Fatalf("state just before the limit = %q", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		r := resultOf(t, a, "limited")
		if r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT ||
			r.GetError().GetMessage() != "stopped after 1h0m0s: the job's time limit was reached" {
			t.Errorf("result = %v", r)
		}
		if got := r.GetFinishedAt().AsTime(); !got.Equal(t0.Add(time.Hour)) {
			t.Errorf("finished at %v, want an hour after the start (%v)", got, t0)
		}
	})
}

func TestTimeLimitAndOtherOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// before happens while the job runs, before the limit is reached
		// (fired after it, if at all).
		before     func(t *testing.T, a *Agent, release chan struct{})
		wantStatus syncrv1.JobResultStatus
		wantCode   syncrv1.JobErrorCode
	}{
		{
			name:       "cancelled before the limit",
			before:     func(t *testing.T, a *Agent, _ chan struct{}) { handle(t, a, cancelCmd("x1", "a")) },
			wantStatus: syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED,
			wantCode:   syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED,
		},
		{
			name:       "finished in time",
			before:     func(_ *testing.T, _ *Agent, release chan struct{}) { close(release) },
			wantStatus: syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var timers fakeTimers
				release := make(chan struct{})
				fr := newFakeRunner(gate(release, time.Second))
				a := newAgent(t, fr, Config{AfterFunc: timers.afterFunc})
				defer shutdown(t, a)

				handle(t, a, limitedStartCmd("c1", "a", time.Minute))
				synctest.Wait()
				tm := timers.only(t)
				tt.before(t, a, release)
				time.Sleep(time.Minute)
				synctest.Wait()
				if !timers.isStopped(tm) {
					t.Error("the limit's timer was not stopped when the job finished")
				}
				timers.fire(tm) // a late limit changes nothing
				synctest.Wait()

				r := resultOf(t, a, "a")
				if r.GetStatus() != tt.wantStatus || r.GetError().GetCode() != tt.wantCode {
					t.Errorf("result = %v, want %v / %v", r, tt.wantStatus, tt.wantCode)
				}
			})
		})
	}
}

func TestNoTimeLimit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var timers fakeTimers
		a := newAgent(t, newFakeRunner(succeed), Config{AfterFunc: timers.afterFunc})
		defer shutdown(t, a)
		handle(t, a, startCmd("c1", "unset"), limitedStartCmd("c2", "zero", 0))
		synctest.Wait()
		if n := timers.count(); n != 0 {
			t.Errorf("%d timers armed for jobs without a limit", n)
		}
		for _, job := range []string{"unset", "zero"} {
			if s := resultOf(t, a, job).GetStatus(); s != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
				t.Errorf("%s: status %v", job, s)
			}
		}
	})
}

func TestInvalidTimeLimitIsRejected(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fr := newFakeRunner(succeed)
		a := newAgent(t, fr, Config{})
		defer shutdown(t, a)
		invalid := startCmd("c2", "invalid")
		invalid.GetStartJob().GetSpec().MaxRuntime = &durationpb.Duration{Seconds: 1, Nanos: -1}
		handle(t, a, limitedStartCmd("c1", "negative", -time.Minute), invalid)
		synctest.Wait()
		for _, job := range []string{"negative", "invalid"} {
			r := resultOf(t, a, job)
			if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
				r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC || r.GetStartedAt() != nil {
				t.Errorf("%s: result = %v", job, r)
			}
			if fr.runCount(job) != 0 {
				t.Errorf("%s ran", job)
			}
		}
	})
}
