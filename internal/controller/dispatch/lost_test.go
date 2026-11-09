package dispatch_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// lostSetup is a worker holding a running job, a dispatched job and a
// running job with a cancel request, plus a queued job it never took, all
// reported at the env clock's start; and another worker with a running job.
type lostSetup struct {
	e                                      *env
	w, o                                   *fakeWorker
	running, dispatched, cancelled, queued jobs.Job
	otherRunning                           jobs.Job
}

func newLostSetup(t *testing.T) *lostSetup {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t)
	wid, oid := e.enroll("nas"), e.enroll("other")
	s := &lostSetup{e: e, w: e.newFakeWorker(wid), o: e.newFakeWorker(oid)}
	s.running, s.dispatched, s.cancelled = e.run(wid), e.run(wid), e.run(wid)
	s.otherRunning = e.run(oid)
	s.w.poll()
	s.w.start(s.running.ID.String())
	s.w.start(s.cancelled.ID.String())
	s.w.report()
	s.o.poll()
	s.o.start(s.otherRunning.ID.String())
	s.o.report()
	s.queued = e.run(wid) // never delivered
	if _, err := e.jobs.Cancel(ctx, testActor(), s.cancelled.ID); err != nil {
		t.Fatal(err)
	}
	return s
}

// expire runs ExpireLost and checks how many jobs it ended.
func (s *lostSetup) expire(t *testing.T, want int) {
	t.Helper()
	n, err := s.e.disp.ExpireLost(context.Background())
	if err != nil || n != want {
		t.Fatalf("ExpireLost = (%d, %v), want %d", n, err, want)
	}
}

func (s *lostSetup) want(t *testing.T, j jobs.Job, status domain.JobStatus, code string) jobs.Job {
	t.Helper()
	got := s.e.job(j.ID)
	if got.Status != status || got.Error.Code != code {
		t.Errorf("job %s = %s %q, want %s %q", j.ID, got.Status, got.Error.Code, status, code)
	}
	return got
}

// TestLostWorkerJobsEnd: the jobs of a worker that stays out of contact do
// not stay active forever. A cancel request that cannot be delivered ends
// the job cancelled after the cancel grace; after LostAfter every
// dispatched and running job ends, interrupted (worker_lost) with a
// notification. Queued jobs are left to the dispatch timeout, and workers
// in contact are not touched.
func TestLostWorkerJobsEnd(t *testing.T) {
	t.Parallel()
	s := newLostSetup(t)
	e := s.e

	e.clock.Advance(5*time.Minute - time.Second)
	s.expire(t, 0)
	e.clock.Advance(time.Second)
	s.expire(t, 1)
	got := s.want(t, s.cancelled, domain.JobCancelled, jobs.CodeCancelled)
	if !strings.Contains(got.Error.Message, "out of contact") || !got.FinishedAt.Equal(e.clock.Now()) {
		t.Errorf("cancelled job = %+v", got)
	}
	if e.rec.failures() != 0 {
		t.Errorf("notifier calls = %d after the cancel", e.rec.failures())
	}

	e.clock.Advance(30 * time.Minute)
	s.o.report() // the other worker stays in contact
	e.clock.Advance(25*time.Minute - time.Second)
	s.expire(t, 0)
	s.want(t, s.running, domain.JobRunning, "")
	e.clock.Advance(time.Second) // exactly LostAfter
	s.expire(t, 2)
	s.expire(t, 0) // idempotent

	for _, j := range []jobs.Job{s.running, s.dispatched} {
		got := s.want(t, j, domain.JobInterrupted, jobs.CodeWorkerLost)
		if !strings.Contains(got.Error.Message, `worker "nas" has been out of contact for more than 1h0m0s (last seen 2026-11-04T12:00:00Z)`) {
			t.Errorf("message = %q", got.Error.Message)
		}
	}
	s.want(t, s.queued, domain.JobQueued, "")
	s.want(t, s.otherRunning, domain.JobRunning, "")
	if n := e.rec.failures(); n != 2 {
		t.Errorf("notifier calls = %d, want 2", n)
	}
	if !strings.Contains(e.logs.String(), "job of a worker out of contact ended") {
		t.Error("ending the jobs was not logged")
	}
}

// TestLostWorkerComesBack: a lost worker that comes back with the same
// epoch delivers late events for jobs the controller already ended. They
// are acknowledged without effect (no error, no loop, no resurrection),
// and the worker is told to stop the jobs it still runs. Both transports
// ingest the same way.
func TestLostWorkerComesBack(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"active", "passive"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := newLostSetup(t)
			e, w := s.e, s.w
			e.clock.Advance(2 * time.Hour)
			s.expire(t, 4) // the other worker is silent too
			before := map[domain.JobID]jobs.Job{}
			for _, j := range []jobs.Job{s.running, s.dispatched, s.cancelled} {
				before[j.ID] = e.job(j.ID)
			}

			// Meanwhile on the worker: the dispatched job started, the
			// running one finished, the others still run.
			w.start(s.dispatched.ID.String())
			w.progress(s.dispatched.ID.String(), 100)
			w.finish(s.running.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
			var cmds []*syncrv1.Command
			switch mode {
			case "active":
				if ack := w.report(); ack != w.seq {
					t.Errorf("ack = %d, want %d (every late event)", ack, w.seq)
				}
				w.report() // a retry changes nothing either
				cmds = w.poll()
			case "passive":
				for range 2 {
					ack, err := e.disp.Ingest(ctx, w.id, w.epoch, w.outbox, w.state())
					if err != nil || ack != w.seq {
						t.Fatalf("Ingest = (%d, %v), want %d", ack, err, w.seq)
					}
				}
				var err error
				if cmds, err = e.disp.Commands(ctx, w.id, w.epoch); err != nil {
					t.Fatal(err)
				}
			}
			for id, was := range before {
				if got := e.job(id); got.Status != was.Status || got.Version != was.Version {
					t.Errorf("job %s changed after coming back: %s v%d, was %s v%d", id, got.Status, got.Version, was.Status, was.Version)
				}
			}
			// The jobs it still runs are stopped; the job that stayed
			// queued is delivered at last.
			var cancels, starts []string
			for _, c := range cmds {
				if id := c.GetCancelJob().GetJobId(); id != "" {
					cancels = append(cancels, id)
				}
				if id := c.GetStartJob().GetJobId(); id != "" {
					starts = append(starts, id)
				}
			}
			slices.Sort(cancels)
			want := []string{s.dispatched.ID.String(), s.cancelled.ID.String()}
			slices.Sort(want)
			if !slices.Equal(cancels, want) || !slices.Equal(starts, []string{s.queued.ID.String()}) {
				t.Errorf("CancelJobs %v and StartJobs %v, want CancelJobs %v and a StartJob for %s", cancels, starts, want, s.queued.ID)
			}
			// The worker is in contact again: nothing more ends.
			s.expire(t, 0)
		})
	}
}

// TestLostCountsFromControllerStart: a controller that was down longer
// than LostAfter does not end jobs at its first sweep; the workers' silence
// counts from its start.
func TestLostCountsFromControllerStart(t *testing.T) {
	t.Parallel()
	s := newLostSetup(t)
	e := s.e
	e.clock.Advance(3 * time.Hour) // the controller is down
	e.restart()
	s.expire(t, 0)
	e.clock.Advance(5 * time.Minute)
	s.expire(t, 1) // the cancel grace, counted from the start
	s.want(t, s.cancelled, domain.JobCancelled, jobs.CodeCancelled)
	e.clock.Advance(55 * time.Minute)
	s.expire(t, 3) // with the other worker's job
	s.want(t, s.running, domain.JobInterrupted, jobs.CodeWorkerLost)
}

// TestCancelGraceCountsFromTheRequest: a cancel requested for a job of a
// worker that is already silent still gets the grace period.
func TestCancelGraceCountsFromTheRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newLostSetup(t)
	e := s.e
	e.clock.Advance(20 * time.Minute)
	s.expire(t, 1) // the job cancelled at the start
	if _, err := e.jobs.Cancel(ctx, testActor(), s.running.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5*time.Minute - time.Second)
	s.expire(t, 0)
	e.clock.Advance(time.Second)
	s.expire(t, 1)
	s.want(t, s.running, domain.JobCancelled, jobs.CodeCancelled)
	s.want(t, s.dispatched, domain.JobDispatched, "")
}

// TestLostThresholdsAreConfigurable: LostAfter is configurable and caps the
// cancel grace.
func TestLostThresholdsAreConfigurable(t *testing.T) {
	t.Parallel()
	s := newLostSetup(t)
	e := s.e
	e.cfg.LostAfter = 2 * time.Minute
	e.restart()
	e.clock.Advance(2*time.Minute - time.Second)
	s.expire(t, 0)
	e.clock.Advance(time.Second)
	s.expire(t, 4)
	s.want(t, s.cancelled, domain.JobCancelled, jobs.CodeCancelled)
	s.want(t, s.running, domain.JobInterrupted, jobs.CodeWorkerLost)
	s.want(t, s.dispatched, domain.JobInterrupted, jobs.CodeWorkerLost)
	s.want(t, s.otherRunning, domain.JobInterrupted, jobs.CodeWorkerLost) // silent too
}

// TestRunEndsLostJobs: the periodic sweep includes lost workers.
func TestRunEndsLostJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLostSetup(t)
		e := s.e
		e.clock.Advance(2 * time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- e.disp.Run(ctx) }()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		s.want(t, s.running, domain.JobInterrupted, jobs.CodeWorkerLost)
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}
