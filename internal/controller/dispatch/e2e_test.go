package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestEndToEnd drives one active worker through the dispatcher's API like
// the real transport would: a clean job, a worker restart mid-job, a
// controller restart mid-job and a lost response.
func TestEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	wid := e.enroll("desktop")
	w := e.newFakeWorker(wid)
	succeeded := syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED

	// 1. queued -> dispatched -> running -> succeeded, watched live.
	a := e.run(wid)
	watched := make(chan []string, 1)
	subscribed := make(chan struct{})
	go func() {
		var seen []string
		err := e.jobs.Watch(ctx, a.ID, func(u jobs.Update) error {
			if len(seen) == 0 {
				close(subscribed) // the first update comes after subscribing
			}
			if u.Job != nil {
				seen = append(seen, string(u.Job.Status))
			} else {
				seen = append(seen, fmt.Sprintf("progress %d", u.Progress.Stats.Bytes))
			}
			return nil
		})
		if err != nil {
			seen = append(seen, err.Error())
		}
		watched <- seen
	}()
	<-subscribed
	w.poll()   // StartJob
	w.report() // ack + Hello
	if got := e.job(a.ID); got.Status != domain.JobDispatched {
		t.Fatalf("A = %s", got.Status)
	}
	w.start(a.ID.String())
	e.clock.Advance(time.Second)
	w.progress(a.ID.String(), 100)
	w.report()
	if got := e.job(a.ID); got.Status != domain.JobRunning {
		t.Fatalf("A = %s", got.Status)
	}
	e.clock.Advance(time.Second)
	w.finish(a.ID.String(), succeeded, nil)
	w.report()
	if got := e.job(a.ID); got.Status != domain.JobSucceeded || got.Stats.Bytes != 1<<33 {
		t.Fatalf("A = %s %+v", got.Status, got.Stats)
	}
	if seen := <-watched; !slices.Equal(seen, []string{"queued", "dispatched", "running", "progress 100", "succeeded"}) {
		t.Errorf("watcher saw %v", seen)
	}
	if samples, _ := e.jobs.Samples(ctx, a.ID); len(samples) != 2 {
		t.Errorf("A has %d stored samples, want 2 (progress + final)", len(samples))
	}

	// 2. The worker restarts while B runs: B is interrupted.
	b := e.run(wid)
	w.poll()
	w.start(b.ID.String())
	w.report()
	firstEpoch := w.epoch
	w.restart()
	w.report()
	if got := e.job(b.ID); got.Status != domain.JobInterrupted || got.Error.Code != jobs.CodeWorkerRestarted {
		t.Fatalf("B = %s %q", got.Status, got.Error.Code)
	}
	if e.rec.failures() != 1 {
		t.Errorf("notifier calls = %d, want 1 (B)", e.rec.failures())
	}
	if wk := e.worker(wid); wk.Link.Epoch != w.epoch || !slices.Contains(wk.Link.PastEpochs, firstEpoch) {
		t.Errorf("link = %+v", wk.Link)
	}

	// 3. The controller restarts while C runs and D is queued.
	c := e.run(wid)
	w.poll()
	w.start(c.ID.String())
	w.progress(c.ID.String(), 5)
	w.report()
	d := e.run(wid)
	e.restart() // new services, broker and hub over the same store

	cmds := w.poll()
	if len(cmds) != 1 || cmds[0].GetStartJob().GetJobId() != d.ID.String() {
		t.Fatalf("after the controller restart: commands = %v, want StartJob D only", cmds)
	}
	w.progress(c.ID.String(), 50)
	w.finish(c.ID.String(), succeeded, nil)
	w.start(d.ID.String())
	w.finish(d.ID.String(), succeeded, nil)
	// The response to this report is lost: the worker sends it all again.
	if _, err := e.disp.Report(ctx, wid, w.reportRequest()); err != nil {
		t.Fatal(err)
	}
	w.report()
	if len(w.outbox) != 0 {
		t.Errorf("worker outbox not drained: %d events", len(w.outbox))
	}
	for _, id := range []domain.JobID{c.ID, d.ID} {
		got := e.job(id)
		if got.Status != domain.JobSucceeded {
			t.Errorf("job %s = %s", id, got.Status)
		}
	}
	if got := e.job(c.ID); got.Version != 4 {
		t.Errorf("C version = %d, want 4 (each change once despite the replay)", got.Version)
	}
	// Nothing left to do.
	if cmds := w.poll(); len(cmds) != 0 {
		t.Errorf("leftover commands: %v", cmds)
	}
	all, _ := e.jobs.List(ctx, jobs.ListFilter{})
	if len(all.Jobs) != 4 {
		t.Errorf("%d jobs", len(all.Jobs))
	}
}

// TestConcurrentLinkAndAPI runs several workers that deliver reports with
// duplicates, reordering, lost responses and restarts, concurrently with API
// calls (create, cancel, get, list, watch). Run it with -count to shake out
// races; every job must end terminal through valid transitions only.
func TestConcurrentLinkAndAPI(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e := newEnv(t)
	const nWorkers, nJobs = 3, 60
	var wids []domain.WorkerID
	for i := range nWorkers {
		wids = append(wids, e.enroll(fmt.Sprintf("w%d", i)))
	}

	var stop atomic.Bool
	var workersWG sync.WaitGroup
	for i, wid := range wids {
		workersWG.Go(func() { runChaosWorker(ctx, t, e, wid, uint64(i), &stop) })
	}

	var apiWG sync.WaitGroup
	var mu sync.Mutex
	var created []domain.JobID
	for g := range 3 {
		apiWG.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(g), 99))
			for range nJobs / 3 {
				j := e.run(wids[rng.IntN(nWorkers)])
				mu.Lock()
				created = append(created, j.ID)
				ids := slices.Clone(created)
				mu.Unlock()
				victim := ids[rng.IntN(len(ids))]
				switch rng.IntN(4) {
				case 0:
					if _, err := e.jobs.Cancel(ctx, victim); err != nil {
						t.Error(err)
					}
				case 1:
					if _, err := e.jobs.List(ctx, jobs.ListFilter{WorkerID: wids[rng.IntN(nWorkers)]}); err != nil {
						t.Error(err)
					}
				case 2:
					apiWG.Go(func() {
						if err := e.jobs.Watch(ctx, victim, func(jobs.Update) error { return nil }); err != nil {
							t.Errorf("watch %s: %v", victim, err)
						}
					})
				}
				time.Sleep(time.Duration(rng.IntN(200)) * time.Microsecond)
			}
		})
	}

	// Wait until every job is finished, then stop the workers.
	for {
		mu.Lock()
		done := len(created) == nJobs
		mu.Unlock()
		if done {
			page, err := e.jobs.List(ctx, jobs.ListFilter{Statuses: []domain.JobStatus{domain.JobQueued, domain.JobDispatched, domain.JobRunning}})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Jobs) == 0 {
				break
			}
		}
		if ctx.Err() != nil {
			t.Fatal("jobs did not finish in time")
		}
		time.Sleep(time.Millisecond)
	}
	stop.Store(true)
	workersWG.Wait()
	apiWG.Wait()

	for _, id := range created {
		hist := e.rec.statuses(id)
		for i := 1; i < len(hist); i++ {
			if !hist[i-1].CanTransitionTo(hist[i]) {
				t.Errorf("job %s published %v: %s -> %s is not a transition", id, hist, hist[i-1], hist[i])
			}
		}
		j := e.job(id)
		if !j.Status.Terminal() || hist[len(hist)-1] != j.Status {
			t.Errorf("job %s ended %s, history %v", id, j.Status, hist)
		}
		samples, _ := e.jobs.Samples(ctx, id)
		for i := 1; i < len(samples); i++ {
			if !samples[i].Time.After(samples[i-1].Time) {
				t.Errorf("job %s samples out of order", id)
			}
		}
	}
}

// runChaosWorker is a worker loop with delivery faults, until stop is set.
func runChaosWorker(ctx context.Context, t *testing.T, e *env, wid domain.WorkerID, seed uint64, stop *atomic.Bool) {
	rng := rand.New(rand.NewPCG(seed, 7))
	w := e.newFakeWorker(wid)
	var old []*syncrv1.ReportRequest
	tolerable := func(err error) bool {
		return err == nil || errors.Is(err, dispatch.ErrStaleEpoch)
	}
	for !stop.Load() && ctx.Err() == nil {
		req := &syncrv1.PollRequest{Epoch: w.epoch, Acks: slices.Clone(w.acks), Wait: durationpb.New(0)}
		resp, err := e.disp.Poll(ctx, wid, req)
		if !tolerable(err) {
			t.Errorf("poll: %v", err)
			return
		}
		if err == nil {
			w.acks = nil
			w.handle(resp.GetCommands())
		}
		for _, id := range slices.Clone(w.held) {
			switch {
			case !w.running[id]:
				w.start(id)
			case rng.IntN(3) == 0:
				st := []syncrv1.JobResultStatus{
					syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED,
					syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED,
					syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED,
				}[rng.IntN(3)]
				w.finish(id, st, nil)
			default:
				e.clock.Advance(time.Millisecond)
				w.progress(id, rng.Int64N(1<<40))
			}
		}
		rep := w.reportRequest()
		old = append(old, rep)
		switch r := rng.IntN(10); {
		case r == 0 && len(old) > 1: // a delayed duplicate of an older request
			_, err = e.disp.Report(ctx, wid, old[rng.IntN(len(old)-1)])
		case r == 1: // the response is lost
			_, err = e.disp.Report(ctx, wid, rep)
		case r == 2: // events reordered in transit
			rep.Events = slices.Clone(rep.GetEvents())
			rng.Shuffle(len(rep.Events), func(i, j int) { rep.Events[i], rep.Events[j] = rep.Events[j], rep.Events[i] })
			_, err = w.deliver(rep)
		case r == 3 && rng.IntN(8) == 0: // the worker process restarts
			w.restart()
		default:
			_, err = w.deliver(rep)
		}
		if !tolerable(err) {
			t.Errorf("report: %v", err)
			return
		}
	}
}
