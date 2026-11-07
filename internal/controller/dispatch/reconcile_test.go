package dispatch_test

import (
	"context"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestEpochRules: when a worker process is replaced, every job the old
// process accepted is reconciled (package doc, "Epoch rules").
func TestEpochRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setup    func(e *env, w *fakeWorker, j jobs.Job)
		cancel   bool
		status   domain.JobStatus
		code     string
		requeues int
		start    bool // a StartJob is derived for the new epoch
		notified bool
	}{
		{name: "queued stays queued", setup: func(*env, *fakeWorker, jobs.Job) {},
			status: domain.JobQueued, start: true},
		{name: "dispatched is re-queued", setup: func(_ *env, w *fakeWorker, _ jobs.Job) { w.poll(); w.report() },
			status: domain.JobQueued, requeues: 1, start: true},
		{name: "dispatched with a cancel request is cancelled", setup: func(_ *env, w *fakeWorker, _ jobs.Job) { w.poll(); w.report() },
			cancel: true, status: domain.JobCancelled, code: jobs.CodeCancelled},
		{name: "running is interrupted", setup: func(_ *env, w *fakeWorker, j jobs.Job) { w.poll(); w.start(j.ID.String()); w.report() },
			status: domain.JobInterrupted, code: jobs.CodeWorkerRestarted, notified: true},
		{name: "running with a cancel request is interrupted", setup: func(_ *env, w *fakeWorker, j jobs.Job) { w.poll(); w.start(j.ID.String()); w.report() },
			cancel: true, status: domain.JobInterrupted, code: jobs.CodeWorkerRestarted, notified: true},
		{name: "finished stays finished", setup: func(_ *env, w *fakeWorker, j jobs.Job) {
			w.poll()
			w.start(j.ID.String())
			w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
			w.report()
		}, status: domain.JobSucceeded},
	}
	triggers := map[string]func(w *fakeWorker){
		"report": func(w *fakeWorker) { w.report() },
		"poll":   func(w *fakeWorker) { w.poll() },
	}
	for _, tt := range tests {
		for trigger, fire := range triggers {
			t.Run(tt.name+"/"+trigger, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				wid := e.enroll("w")
				j := e.run(wid)
				w := e.newFakeWorker(wid)
				tt.setup(e, w, j)
				if tt.cancel {
					if _, err := e.jobs.Cancel(context.Background(), testActor(), j.ID); err != nil {
						t.Fatal(err)
					}
				}
				w.restart()
				fire(w)
				got := e.job(j.ID)
				if trigger == "poll" && tt.start {
					// The poll itself delivered the re-derived StartJob.
					if !slices.Contains(w.held, j.ID.String()) {
						t.Errorf("new process did not receive StartJob")
					}
					w.report()
					got = e.job(j.ID)
					if got.Status != domain.JobDispatched || got.Epoch != w.epoch {
						t.Errorf("after the new process acked: %s epoch %q", got.Status, got.Epoch)
					}
					return
				}
				if got.Status != tt.status || got.Error.Code != tt.code || got.Requeues != tt.requeues {
					t.Errorf("job = %s %q requeues %d, want %s %q %d", got.Status, got.Error.Code, got.Requeues, tt.status, tt.code, tt.requeues)
				}
				if (e.rec.failures() > 0) != tt.notified {
					t.Errorf("notifier calls = %d", e.rec.failures())
				}
				cmds, err := e.disp.Commands(context.Background(), wid, w.epoch)
				hasStart := slices.ContainsFunc(cmds, func(c *syncrv1.Command) bool { return c.GetStartJob() != nil })
				if err != nil || hasStart != tt.start {
					t.Errorf("commands for the new epoch = %v (%v), want StartJob %v", cmds, err, tt.start)
				}
				for i := 1; i < len(e.rec.statuses(j.ID)); i++ {
					st := e.rec.statuses(j.ID)
					if !st[i-1].CanTransitionTo(st[i]) {
						t.Errorf("invalid transition published: %v", st)
					}
				}
			})
		}
	}
}

// TestSnapshotRules covers the snapshot table of the package doc.
func TestSnapshotRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("listed queued job counts as accepted", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		w := e.newFakeWorker(wid)
		w.poll()
		w.acks = nil // the ack is lost; the snapshot still proves acceptance
		w.report()
		if got := e.job(j.ID); got.Status != domain.JobDispatched || got.Epoch != w.epoch {
			t.Errorf("job = %s %q", got.Status, got.Epoch)
		}
	})

	t.Run("listed finished, unknown and foreign jobs are cancelled until acked", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid, other := e.enroll("w"), e.enroll("other")
		finished := e.run(wid)
		foreign := e.run(other)
		unknown := domain.NewJobID().String()
		w := e.newFakeWorker(wid)
		w.poll()
		if _, err := e.jobs.Cancel(ctx, testActor(), finished.ID); err != nil { // concurrent with the delivery
			t.Fatal(err)
		}
		if got := e.job(finished.ID); got.Status != domain.JobCancelled {
			t.Fatalf("job = %s", got.Status)
		}
		w.held = append(w.held, foreign.ID.String(), unknown) // a confused worker
		w.report()
		cmds, _ := e.disp.Commands(ctx, wid, w.epoch)
		var cancels []string
		for _, c := range cmds {
			cancels = append(cancels, c.GetCancelJob().GetJobId())
			if c.GetId() != "cancel:"+c.GetCancelJob().GetJobId() {
				t.Errorf("command id %q", c.GetId())
			}
		}
		if want := []string{finished.ID.String(), foreign.ID.String(), unknown}; !slices.Equal(cancels, want) {
			t.Fatalf("cancels = %v, want %v", cancels, want)
		}
		if got := e.job(foreign.ID); got.Status != domain.JobQueued {
			t.Errorf("the foreign job changed: %s", got.Status)
		}
		// Acked cancels are not sent again while the worker stops the jobs.
		w.poll()
		w.held = []string{unknown} // still stopping one
		w.report()
		if cmds, _ := e.disp.Commands(ctx, wid, w.epoch); len(cmds) != 0 {
			t.Errorf("acked cancels were resent: %v", cmds)
		}
		// The stopped job's result is ignored: it is already cancelled.
		if got := e.job(finished.ID); got.Status != domain.JobCancelled || got.Error.Code != jobs.CodeCancelled {
			t.Errorf("job = %s %q", got.Status, got.Error.Code)
		}
	})

	tests := []struct {
		name   string
		mutate func(req *syncrv1.ReportRequest, w *fakeWorker)
		status domain.JobStatus
	}{
		{"running job missing from a current snapshot is lost", func(*syncrv1.ReportRequest, *fakeWorker) {}, domain.JobInterrupted},
		{"missing from a stale snapshot: nothing", func(req *syncrv1.ReportRequest, _ *fakeWorker) {
			req.State.LastSeq--
		}, domain.JobRunning},
		{"missing, position unknown, no events: nothing", func(req *syncrv1.ReportRequest, _ *fakeWorker) {
			req.State.LastSeq = 0
		}, domain.JobRunning},
		{"missing, position from the envelope's last event", func(req *syncrv1.ReportRequest, w *fakeWorker) {
			req.State.LastSeq = 0
			w.progress("ignored", 1)
			req.Events = append(req.Events, w.outbox[len(w.outbox)-1])
		}, domain.JobInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wid := e.enroll("w")
			j := e.run(wid)
			w := e.newFakeWorker(wid)
			w.poll()
			w.start(j.ID.String())
			w.report()
			w.held = nil // the worker silently dropped the job (a bug)
			req := w.reportRequest()
			tt.mutate(req, w)
			if _, err := e.disp.Report(ctx, wid, req); err != nil {
				t.Fatal(err)
			}
			got := e.job(j.ID)
			if got.Status != tt.status || (tt.status == domain.JobInterrupted && got.Error.Code != jobs.CodeWorkerLost) {
				t.Errorf("job = %s %q, want %s", got.Status, got.Error.Code, tt.status)
			}
		})
	}

	t.Run("dispatched job missing from the snapshot is left alone", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		w := e.newFakeWorker(wid)
		// The snapshot was taken before the worker accepted the job; the
		// ack overtook it.
		early := w.reportRequest()
		w.poll()
		w.report()
		if _, err := e.disp.Report(ctx, wid, early); err != nil {
			t.Fatal(err)
		}
		if got := e.job(j.ID); got.Status != domain.JobDispatched {
			t.Errorf("job = %s", got.Status)
		}
	})

	t.Run("result in the same envelope wins over the missing rule", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		w := e.newFakeWorker(wid)
		w.poll()
		w.start(j.ID.String())
		w.report()
		w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
		w.report()
		if got := e.job(j.ID); got.Status != domain.JobSucceeded {
			t.Errorf("job = %s", got.Status)
		}
	})
}

// TestCancelThroughTheLink cancels at every state and follows the commands
// to the worker and back.
func TestCancelThroughTheLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("queued: cancelled, never started", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		w := e.newFakeWorker(wid)
		if _, err := e.jobs.Cancel(ctx, testActor(), j.ID); err != nil {
			t.Fatal(err)
		}
		if cmds := w.poll(); len(cmds) != 0 {
			t.Errorf("commands after cancel = %v", cmds)
		}
		if got := e.job(j.ID); got.Status != domain.JobCancelled {
			t.Errorf("job = %s", got.Status)
		}
	})

	for _, running := range []bool{false, true} {
		name := map[bool]string{false: "dispatched", true: "running"}[running]
		t.Run(name+": CancelJob until acked, then cancelled", func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			wid := e.enroll("w")
			j := e.run(wid)
			w := e.newFakeWorker(wid)
			w.poll()
			if running {
				w.start(j.ID.String())
			}
			w.report()
			if _, err := e.jobs.Cancel(ctx, testActor(), j.ID); err != nil {
				t.Fatal(err)
			}
			if got := e.job(j.ID); !got.CancelRequested() || got.Status.Terminal() {
				t.Fatalf("after cancel: %s requested %v", got.Status, got.CancelRequested())
			}
			cmds := w.poll() // delivers CancelJob; the fake stops the job at once
			if len(cmds) != 1 || cmds[0].GetCancelJob().GetJobId() != j.ID.String() {
				t.Fatalf("commands = %v", cmds)
			}
			if again := w.poll(); len(again) != 0 { // the ack goes with this poll
				t.Errorf("CancelJob resent after the ack: %v", again)
			}
			w.report()
			if got := e.job(j.ID); got.Status != domain.JobCancelled || got.Error.Code != jobs.CodeCancelled {
				t.Errorf("job = %s %q", got.Status, got.Error.Code)
			}
		})
	}

	t.Run("running: the worker finishes before the cancel arrives", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		w := e.newFakeWorker(wid)
		w.poll()
		w.start(j.ID.String())
		w.report()
		if _, err := e.jobs.Cancel(ctx, testActor(), j.ID); err != nil {
			t.Fatal(err)
		}
		w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
		w.report()
		w.poll() // CancelJob for a finished job: a no-op on the worker
		if got := e.job(j.ID); got.Status != domain.JobSucceeded {
			t.Errorf("job = %s", got.Status)
		}
		if cmds, _ := e.disp.Commands(ctx, wid, w.epoch); len(cmds) != 0 {
			t.Errorf("commands = %v", cmds)
		}
	})

	t.Run("finished: nothing", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		wid := e.enroll("w")
		j := e.run(wid)
		w := e.newFakeWorker(wid)
		w.poll()
		w.start(j.ID.String())
		w.finish(j.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED, &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED})
		w.report()
		before := e.job(j.ID)
		got, err := e.jobs.Cancel(ctx, testActor(), j.ID)
		if err != nil || got.Version != before.Version || got.Status != domain.JobFailed {
			t.Errorf("Cancel(finished) = (%s v%d, %v)", got.Status, got.Version, err)
		}
		if cmds := w.poll(); len(cmds) != 0 {
			t.Errorf("commands = %v", cmds)
		}
	})
}
