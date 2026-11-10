package dispatch_test

import (
	"context"
	"slices"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// runKeyed queues a transfer on w that holds the concurrency keys.
func (e *env) runKeyed(w domain.WorkerID, keys ...string) jobs.Job {
	e.t.Helper()
	def := testDefinition("/data")
	kind, refs, err := kinds(e.t).Check(def)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := jobkind.Marshal(def)
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := e.jobs.Create(context.Background(), testActor(), jobs.CreateParams{
		WorkerID: w, Kind: kind, Definition: raw, RemoteIDs: refs.RemoteIDs, ConcurrencyKeys: keys,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// started returns the job IDs of the StartJob commands in cmds.
func started(cmds []*syncrv1.Command) []string {
	var out []string
	for _, c := range cmds {
		if s := c.GetStartJob(); s != nil {
			out = append(out, s.GetJobId())
		}
	}
	return out
}

// TestConcurrencyKeysSerializeAcrossWorkers: of the unfinished jobs
// sharing a key only the oldest is sent to its worker; the next one
// starts, on whatever worker, once it finished.
func TestConcurrencyKeysSerializeAcrossWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w1, w2 := e.newFakeWorker(e.enroll("one")), e.newFakeWorker(e.enroll("two"))
	w1.report()
	w2.report()

	head := e.runKeyed(w1.id, "restic-repo:a")
	second := e.runKeyed(w2.id, "restic-repo:a")
	third := e.runKeyed(w1.id, "restic-repo:a", "restic-repo:b")
	free := e.run(w2.id)
	otherKey := e.runKeyed(w2.id, "restic-repo:c")

	if got, want := started(w2.poll()), []string{free.ID.String(), otherKey.ID.String()}; !slices.Equal(got, want) {
		t.Fatalf("worker two starts %v, want %v (its keyed job waits for the head)", got, want)
	}
	if got, want := started(w1.poll()), []string{head.ID.String()}; !slices.Equal(got, want) {
		t.Fatalf("worker one starts %v, want %v", got, want)
	}

	// The head is dispatched, then running: the others still wait, also
	// after a controller restart (nothing is kept in memory).
	w1.report()
	w1.start(head.ID.String())
	w1.report()
	e.restart()
	if got := started(w2.poll()); len(got) != 0 {
		t.Fatalf("worker two starts %v while the head runs", got)
	}
	if got := started(w1.poll()); len(got) != 0 {
		t.Fatalf("worker one starts %v while the head runs", got)
	}
	blockers, err := e.jobs.Blockers(context.Background(), []domain.JobID{second.ID, third.ID})
	if err != nil || blockers[second.ID] != head.ID || blockers[third.ID] != head.ID {
		t.Fatalf("blockers = %v, %v; want both to wait for the head", blockers, err)
	}

	// The head finishes: worker two is woken and gets the second job.
	woken := make(chan []*syncrv1.Command, 1)
	go func() {
		cmds, _ := e.disp.WaitCommands(context.Background(), w2.id, w2.epoch, 10*time.Second)
		woken <- cmds
	}()
	w1.finish(head.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
	w1.report()
	select {
	case cmds := <-woken:
		if got, want := started(cmds), []string{second.ID.String()}; !slices.Equal(got, want) {
			t.Fatalf("after the head finished worker two starts %v, want %v", got, want)
		}
		w2.handle(cmds)
	case <-time.After(5 * time.Second):
		t.Fatal("worker two was not woken when the head finished")
	}
	if got := started(w1.poll()); len(got) != 0 {
		t.Fatalf("worker one starts %v behind the second job", got)
	}

	// A cancelled waiter gives way too.
	w2.report()
	if _, err := e.jobs.Cancel(context.Background(), testActor(), second.ID); err != nil {
		t.Fatal(err)
	}
	w2.poll() // the CancelJob
	w2.report()
	if got := e.job(second.ID).Status; got != domain.JobCancelled {
		t.Fatalf("second job = %s", got)
	}
	if got, want := started(w1.poll()), []string{third.ID.String()}; !slices.Equal(got, want) {
		t.Fatalf("worker one starts %v, want %v", got, want)
	}
}

// TestWaitingKeyedJobDoesNotTimeOut: the dispatch timeout of a job behind
// an older sharer starts when it becomes the head.
func TestWaitingKeyedJobDoesNotTimeOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newFakeWorker(e.enroll("one"))
	w.report()
	head := e.runKeyed(w.id, "restic-repo:a")
	waiter := e.runKeyed(w.id, "restic-repo:a")
	w.poll()
	w.report()
	w.start(head.ID.String())
	w.report()

	e.clock.Advance(e.cfg.DispatchTimeout + time.Minute)
	w.report() // still in contact
	if n, err := e.disp.ExpireQueued(context.Background()); err != nil || n != 0 {
		t.Fatalf("ExpireQueued = %d, %v; want nothing to expire", n, err)
	}
	if got := e.job(waiter.ID).Status; got != domain.JobQueued {
		t.Fatalf("waiter = %s", got)
	}

	// The head ends; the waiter is never taken (the worker stops polling).
	w.finish(head.ID.String(), syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED, nil)
	w.report()
	e.clock.Advance(e.cfg.DispatchTimeout - time.Minute)
	if n, err := e.disp.ExpireQueued(context.Background()); err != nil || n != 0 {
		t.Fatalf("before a full timeout as head: ExpireQueued = %d, %v", n, err)
	}
	e.clock.Advance(2 * time.Minute)
	if n, err := e.disp.ExpireQueued(context.Background()); err != nil || n != 1 {
		t.Fatalf("after a full timeout as head: ExpireQueued = %d, %v", n, err)
	}
	if got := e.job(waiter.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeDispatchTimeout {
		t.Fatalf("waiter = %s %q", got.Status, got.Error.Code)
	}
}
