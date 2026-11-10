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
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// resticEnv is an env whose dispatcher builds restic backups (backupKind,
// which needs link.FeatureResticV2) on one repository of testOwner.
func resticEnv(t *testing.T) (*env, domain.ResticRepoID) {
	t.Helper()
	repoID := domain.NewResticRepoID()
	repos := &fakeRepos{repo: restic.Resolved{ID: repoID, OwnerID: testOwner, RemoteID: testRemote(), Password: testRepoPassword}}
	e := newEnv(t)
	specs, _ := repoSpecs(t, repos)
	disp, err := dispatch.New(dispatch.Deps{Jobs: e.jobs, Workers: e.workers, Specs: specs, Probes: e.probes, Clock: e.clock, Hub: e.hub}, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.disp = disp
	return e, repoID
}

func (e *env) queueBackup(w domain.WorkerID, repo domain.ResticRepoID) jobs.Job {
	e.t.Helper()
	j, err := e.jobs.Create(context.Background(), testActor(), jobs.CreateParams{
		WorkerID: w, Kind: "restic_backup", Definition: backupJob(e.t, repo).Definition,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// upgrade restarts w as a Milestone 5 worker (every link feature), downgrade
// as an older one (none).
func (w *fakeWorker) upgrade() {
	w.features, w.probeOps = link.Features(), link.ProbeOps()
	w.restart()
}

func (w *fakeWorker) downgrade() {
	w.features, w.probeOps = nil, nil
	w.restart()
}

// TestNewProcessWaitsForItsHello: the stored Info belongs to the process
// that sent it. When a new process's first poll overtakes the Report that
// carries its Hello, the dispatcher decides nothing from the previous
// process's Info: it holds the StartJob back (the job stays queued) until
// the new Hello is stored, then decides on it.
func TestNewProcessWaitsForItsHello(t *testing.T) {
	t.Parallel()

	t.Run("upgrade: the job is not failed on the old Hello", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		w := e.newFakeWorker(e.enroll("nas")) // Milestone 4: no restic.v2
		w.report()
		j := e.queueBackup(w.id, repo) // queued while the worker is down for its upgrade
		w.upgrade()
		if cmds := w.poll(); len(cmds) != 0 {
			t.Fatalf("before the new Hello: %v", cmds)
		}
		if got := e.job(j.ID); got.Status != domain.JobQueued {
			t.Fatalf("before the new Hello the job is %s %+v, want queued", got.Status, got.Error)
		}
		if wk := e.worker(w.id); wk.InfoCurrent() || wk.Link.Epoch != w.epoch {
			t.Fatalf("worker = %+v, want the new epoch without its Info", wk.Link)
		}
		w.report() // the Hello
		if got := started(w.poll()); !slices.Equal(got, []string{j.ID.String()}) {
			t.Fatalf("after the new Hello: StartJob %v, want %s", got, j.ID)
		}
		if spec := w.specs[j.ID.String()]; spec.GetResticBackup() == nil {
			t.Errorf("spec = %v", spec)
		}
	})

	t.Run("downgrade: the job is not sent on the new process's old Hello", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		w := e.newProbeWorker(e.enroll("nas"))
		w.report()
		j := e.queueBackup(w.id, repo)
		w.downgrade()
		if cmds := w.poll(); len(cmds) != 0 {
			t.Fatalf("before the new Hello: %v", cmds)
		}
		if got := e.job(j.ID); got.Status != domain.JobQueued {
			t.Fatalf("before the new Hello the job is %s", got.Status)
		}
		w.report()
		if cmds := w.poll(); len(cmds) != 0 {
			t.Fatalf("after the new Hello: %v", cmds)
		}
		if got := e.job(j.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeWorkerOutdated {
			t.Errorf("job = %s %+v, want failed with worker_outdated", got.Status, got.Error)
		}
	})

	t.Run("first connection: enrollment info is not a Hello", func(t *testing.T) {
		t.Parallel()
		e, repo := resticEnv(t)
		wid := e.enroll("nas")
		j := e.queueBackup(wid, repo)
		w := e.newProbeWorker(wid)
		if cmds := w.poll(); len(cmds) != 0 || e.job(j.ID).Status != domain.JobQueued {
			t.Fatalf("before the first Hello: %v, job %s", cmds, e.job(j.ID).Status)
		}
		w.report()
		if got := started(w.poll()); !slices.Equal(got, []string{j.ID.String()}) {
			t.Fatalf("after the first Hello: %v", got)
		}
	})

	t.Run("probes wait for the Hello too", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			w := e.newProbeWorker(e.enroll("nas"))
			w.report()
			w.restart()
			w.poll() // adopts the new epoch
			answer := goProbe(t.Context(), e, w.id, runProbe("p1"))
			synctest.Wait()
			if got := probeCommands(w.poll()); len(got) != 0 {
				t.Fatalf("before the new Hello: %v", got)
			}
			w.report()
			if got := probeCommands(w.poll()); !slices.Equal(got, []string{"probe:p1"}) {
				t.Fatalf("after the new Hello: %v", got)
			}
			if err := w.complete(okResult("p1")); err != nil {
				t.Fatal(err)
			}
			if a := <-answer; a.err != nil {
				t.Fatal(a.err)
			}
		})
	})

	t.Run("cancels do not wait", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		e := newEnv(t)
		wid := e.enroll("nas")
		e.run(wid)
		unknown := domain.NewJobID().String()
		// A snapshot listing a job the controller does not know, delivered
		// without the Hello (a worker that lost it).
		if _, err := e.disp.Ingest(ctx, wid, "ep", nil, &syncrv1.WorkerState{Jobs: []*syncrv1.ActiveJob{{JobId: unknown}}}); err != nil {
			t.Fatal(err)
		}
		cmds, err := e.disp.Commands(ctx, wid, "ep")
		if err != nil || len(cmds) != 1 || cmds[0].GetCancelJob().GetJobId() != unknown {
			t.Fatalf("Commands = (%v, %v), want the orphan's CancelJob only", cmds, err)
		}
	})
}

// TestHelloWakesTheHeldPoll: a long poll that found its StartJob held back
// returns it as soon as the Report with the Hello is stored.
func TestHelloWakesTheHeldPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, repo := resticEnv(t)
		w := e.newFakeWorker(e.enroll("nas"))
		w.report()
		j := e.queueBackup(w.id, repo)
		w.upgrade()
		ch := waitAsync(t.Context(), e, w.id, w.epoch, 20*time.Second)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case r := <-ch:
			t.Fatalf("the poll returned before the Hello: %+v", r)
		default:
		}
		w.report()
		r := <-ch
		if r.err != nil || len(r.cmds) != 1 || r.cmds[0].GetStartJob().GetJobId() != j.ID.String() || r.elapsed != 3*time.Second {
			t.Errorf("result = %+v, want the StartJob after 3s", r)
		}
	})
}

// TestPassiveRoundWaitsForTheHello: a passive round derives commands with
// the same rule. A Pull of a new process that does not carry its Hello sends
// nothing; the next one, with the Hello, sends the job.
func TestPassiveRoundWaitsForTheHello(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, repo := resticEnv(t)
	wid := registerPassive(t, e)
	c := &fakePassive{w: e.newFakeWorker(wid), start: time.Now()}
	loop := e.disp.NewPassiveLoop(wid, c)
	if _, _, err := loop.Round(ctx); err != nil { // the Milestone 4 Hello
		t.Fatal(err)
	}
	j := e.queueBackup(wid, repo)
	var hello *syncrv1.Event
	c.do(func(w *fakeWorker) {
		w.upgrade()
		hello, w.outbox = w.outbox[0], nil // not in the first Pull
	})
	if _, _, err := loop.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if calls, _ := c.history(); slices.Contains(calls, "command") {
		t.Fatalf("calls = %v, want no command before the Hello", calls)
	}
	if got := e.job(j.ID); got.Status != domain.JobQueued {
		t.Fatalf("job = %s %+v, want queued", got.Status, got.Error)
	}
	c.do(func(w *fakeWorker) { w.outbox = []*syncrv1.Event{hello} })
	if _, _, err := loop.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.job(j.ID); got.Status != domain.JobDispatched {
		t.Errorf("after the Hello the job is %s %+v, want dispatched", got.Status, got.Error)
	}
}

// TestHelloThatNeverComes: a process that never sends its Hello wedges
// nothing for good: its queued jobs fail with the dispatch timeout, saying
// why.
func TestHelloThatNeverComes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("nas")
	j := e.run(wid)
	if cmds, err := e.disp.Commands(ctx, wid, "silent"); err != nil || len(cmds) != 0 {
		t.Fatalf("Commands = (%v, %v)", cmds, err)
	}
	e.clock.Advance(16 * time.Minute)
	if _, err := e.disp.Commands(ctx, wid, "silent"); err != nil { // still in contact
		t.Fatal(err)
	}
	if n, err := e.disp.ExpireQueued(ctx); err != nil || n != 1 {
		t.Fatalf("ExpireQueued = (%d, %v), want 1", n, err)
	}
	got := e.job(j.ID)
	if got.Status != domain.JobFailed || got.Error.Code != jobs.CodeDispatchTimeout ||
		!strings.HasSuffix(got.Error.Message, `worker "nas" is online but its process has not reported what it can run`) {
		t.Errorf("job = %s %+v", got.Status, got.Error)
	}
}
