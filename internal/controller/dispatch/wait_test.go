package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

type waitResult struct {
	cmds    []*syncrv1.Command
	err     error
	elapsed time.Duration
}

// waitAsync starts WaitCommands in the bubble and returns its result
// channel. Time in the bubble only advances when every goroutine is
// blocked, so elapsed is exact.
func waitAsync(ctx context.Context, e *env, wid domain.WorkerID, epoch string, wait time.Duration) <-chan waitResult {
	ch := make(chan waitResult, 1)
	go func() {
		start := time.Now()
		cmds, err := e.disp.WaitCommands(ctx, wid, epoch, wait)
		ch <- waitResult{cmds, err, time.Since(start)}
	}()
	return ch
}

func TestWaitCommands(t *testing.T) {
	t.Parallel()
	const ep = "epoch-1"

	t.Run("returns at once when commands exist", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid := e.enroll("w")
			e.hello(wid, ep)
			e.run(wid)
			r := <-waitAsync(t.Context(), e, wid, ep, 20*time.Second)
			if r.err != nil || len(r.cmds) != 1 || r.elapsed != 0 {
				t.Errorf("result = %+v", r)
			}
		})
	})

	t.Run("woken by a new job", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid := e.enroll("w")
			e.hello(wid, ep)
			ch := waitAsync(t.Context(), e, wid, ep, 20*time.Second)
			time.Sleep(5 * time.Second)
			synctest.Wait()
			j := e.run(wid)
			r := <-ch
			if r.err != nil || len(r.cmds) != 1 || r.cmds[0].GetStartJob().GetJobId() != j.ID.String() || r.elapsed != 5*time.Second {
				t.Errorf("result = %+v", r)
			}
		})
	})

	t.Run("woken by a cancel request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid := e.enroll("w")
			j := e.run(wid)
			w := e.newFakeWorker(wid)
			w.report()
			w.poll()
			w.report() // dispatched; no commands left
			ch := waitAsync(t.Context(), e, wid, w.epoch, 20*time.Second)
			time.Sleep(3 * time.Second)
			synctest.Wait()
			if _, err := e.jobs.Cancel(t.Context(), testActor(), j.ID); err != nil {
				t.Fatal(err)
			}
			r := <-ch
			if r.err != nil || len(r.cmds) != 1 || r.cmds[0].GetCancelJob() == nil || r.elapsed != 3*time.Second {
				t.Errorf("result = %+v", r)
			}
		})
	})

	t.Run("not woken by another worker's job", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid, other := e.enroll("w"), e.enroll("other")
			ch := waitAsync(t.Context(), e, wid, ep, 10*time.Second)
			synctest.Wait()
			e.run(other)
			r := <-ch
			if r.err != nil || len(r.cmds) != 0 || r.elapsed != 10*time.Second {
				t.Errorf("result = %+v", r)
			}
		})
	})

	t.Run("wait elapses, capped at MaxWait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid := e.enroll("w")
			for _, tc := range []struct{ wait, want time.Duration }{{10 * time.Second, 10 * time.Second}, {time.Hour, 25 * time.Second}, {0, 0}, {-time.Second, 0}} {
				r := <-waitAsync(t.Context(), e, wid, ep, tc.wait)
				if r.err != nil || r.cmds != nil || r.elapsed != tc.want {
					t.Errorf("wait %v: result = %+v, want empty after %v", tc.wait, r, tc.want)
				}
			}
		})
	})

	t.Run("ends with the context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid := e.enroll("w")
			ctx, cancel := context.WithCancel(t.Context())
			ch := waitAsync(ctx, e, wid, ep, 20*time.Second)
			time.Sleep(time.Second)
			cancel()
			r := <-ch
			if !errors.Is(r.err, context.Canceled) || r.elapsed != time.Second {
				t.Errorf("result = %+v", r)
			}
		})
	})

	t.Run("Poll applies acks first and defaults to MaxWait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			wid := e.enroll("w")
			j := e.run(wid)
			start := time.Now()
			resp, err := e.disp.Poll(t.Context(), wid, &syncrv1.PollRequest{
				Epoch: ep, Acks: []*syncrv1.CommandAck{{CommandId: "start:" + j.ID.String()}},
			})
			if err != nil || len(resp.GetCommands()) != 0 || time.Since(start) != 25*time.Second {
				t.Errorf("Poll = (%v, %v) after %v, want empty after 25s", resp, err, time.Since(start))
			}
			resp, err = e.disp.Poll(t.Context(), wid, &syncrv1.PollRequest{Epoch: ep, Wait: durationpb.New(time.Second)})
			if err != nil || len(resp.GetCommands()) != 0 {
				t.Errorf("Poll = (%v, %v)", resp, err)
			}
		})
	})
}

func TestDispatchTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	pending := e.enroll("never-connected")
	offline := e.enroll("laptop")
	online := e.enroll("server")
	wOff, wOn := e.newFakeWorker(offline), e.newFakeWorker(online)
	wOff.report()

	jPending, jOffline := e.run(pending), e.run(offline)
	jOnline := e.run(online)
	e.clock.Advance(10 * time.Minute)
	jLater := e.run(offline) // queued 10 minutes after the others
	jDispatched := e.run(online)
	wOn.report() // its Hello
	wOn.poll()
	wOn.acks = wOn.acks[len(wOn.acks)-1:] // only jDispatched is accepted
	wOn.held = []string{jDispatched.ID.String()}
	wOn.report()

	e.clock.Advance(5 * time.Minute) // exactly the timeout: not yet "longer than"
	if n, err := e.disp.ExpireQueued(ctx); err != nil || n != 0 {
		t.Fatalf("at the timeout: ExpireQueued = (%d, %v)", n, err)
	}
	e.clock.Advance(time.Microsecond)
	wOn.report() // the online worker keeps reporting but never accepts jOnline
	n, err := e.disp.ExpireQueued(ctx)
	if err != nil || n != 3 {
		t.Fatalf("ExpireQueued = (%d, %v), want 3", n, err)
	}
	for _, tc := range []struct {
		job  jobs.Job
		want string
	}{
		{jPending, `worker "never-connected" has never connected`},
		{jOffline, `worker "laptop" is offline (last seen 2026-11-04T12:00:00Z)`},
		{jOnline, `worker "server" is online but did not accept the job`},
	} {
		got := e.job(tc.job.ID)
		if got.Status != domain.JobFailed || got.Error.Code != jobs.CodeDispatchTimeout ||
			!strings.HasPrefix(got.Error.Message, "not dispatched within 15m0s: ") || !strings.HasSuffix(got.Error.Message, tc.want) {
			t.Errorf("job %s = %s %+v, want ...%s", tc.job.ID, got.Status, got.Error, tc.want)
		}
	}
	if got := e.job(jLater.ID); got.Status != domain.JobQueued {
		t.Errorf("a job queued later = %s", got.Status)
	}
	if got := e.job(jDispatched.ID); got.Status != domain.JobDispatched {
		t.Errorf("a dispatched job = %s", got.Status)
	}
	if e.rec.failures() != 3 {
		t.Errorf("notifier calls = %d", e.rec.failures())
	}
}

// TestDispatchTimeoutCountsFromControllerStart: after a controller outage
// longer than the dispatch timeout, the first sweep does not fail the
// queued jobs before their workers had a chance to reconnect; their time
// in the queue counts from the controller's start.
func TestDispatchTimeoutCountsFromControllerStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	back, gone := e.enroll("back"), e.enroll("gone")
	wBack, wGone := e.newFakeWorker(back), e.newFakeWorker(gone)
	wBack.report()
	wGone.report()
	jBack, jGone := e.run(back), e.run(gone)

	e.clock.Advance(3 * time.Hour) // the controller is down
	e.restart()
	if n, err := e.disp.ExpireQueued(ctx); err != nil || n != 0 {
		t.Fatalf("first sweep after the restart: ExpireQueued = (%d, %v), want 0", n, err)
	}
	e.clock.Advance(40 * time.Second) // one worker reconnects
	if cmds := wBack.poll(); len(cmds) != 1 || cmds[0].GetStartJob().GetJobId() != jBack.ID.String() {
		t.Fatalf("the reconnected worker got %v", cmds)
	}
	wBack.report()

	e.clock.Advance(15*time.Minute - 40*time.Second) // exactly the timeout since the start
	if n, err := e.disp.ExpireQueued(ctx); err != nil || n != 0 {
		t.Fatalf("at the timeout: ExpireQueued = (%d, %v), want 0", n, err)
	}
	e.clock.Advance(time.Microsecond)
	if n, err := e.disp.ExpireQueued(ctx); err != nil || n != 1 {
		t.Fatalf("after the timeout: ExpireQueued = (%d, %v), want 1", n, err)
	}
	if got := e.job(jGone.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeDispatchTimeout ||
		!strings.HasSuffix(got.Error.Message, `worker "gone" is offline (last seen 2026-11-04T12:00:00Z)`) {
		t.Errorf("job of the worker that stayed away = %s %+v", got.Status, got.Error)
	}
	if got := e.job(jBack.ID); got.Status != domain.JobDispatched {
		t.Errorf("job of the worker that came back = %s", got.Status)
	}
}

func TestRequeueRestartsTheDispatchClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("w")
	j := e.run(wid)
	w := e.newFakeWorker(wid)
	w.report() // its Hello
	w.poll()
	w.report()
	e.clock.Advance(20 * time.Minute)
	w.restart()
	w.report() // re-queued now
	e.clock.Advance(10 * time.Minute)
	if n, _ := e.disp.ExpireQueued(ctx); n != 0 {
		t.Errorf("a freshly re-queued job timed out")
	}
	if got := e.job(j.ID); got.Status != domain.JobQueued || got.Requeues != 1 {
		t.Errorf("job = %s requeues %d", got.Status, got.Requeues)
	}
}

func TestRunSweepsPeriodically(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		e.clock.Advance(time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- e.disp.Run(ctx) }()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if got := e.job(j.ID); got.Status != domain.JobFailed {
			t.Errorf("after one sweep interval: %s", got.Status)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}
