package e2e

import (
	"context"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// runKeyed queues a job of the owner on worker w that holds keys (as every
// restic job holds its repository's).
func (c *controller) runKeyed(w domain.WorkerID, def *syncrv1.JobDefinition, keys ...string) domain.JobID {
	c.t.Helper()
	kind, refs, err := kinds(c.t).Check(def)
	if err != nil {
		c.t.Fatal(err)
	}
	js, _ := c.svc()
	raw, err := jobkind.Marshal(def)
	if err != nil {
		c.t.Fatal(err)
	}
	j, err := js.Create(context.Background(), testActor(), jobs.CreateParams{
		WorkerID: w, Kind: kind, Definition: raw, RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms, ConcurrencyKeys: keys,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return j.ID
}

// TestConcurrencyKeysAcrossWorkers: jobs that share a key on two active
// workers run one at a time, first in first out, also across a controller
// restart; a job without the key is not held up.
func TestConcurrencyKeysAcrossWorkers(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id1, secret1 := c.enrollActive("one")
	id2, secret2 := c.enrollActive("two")
	rn1, rn2 := newFakeRunner(), newFakeRunner()
	startActive(t, rn1, activeOptions(c, id1, secret1))
	startActive(t, rn2, activeOptions(c, id2, secret2))

	const key = "restic-repo:e2e"
	first := c.runKeyed(id1, heldDefinition(), key)
	second := c.runKeyed(id2, heldDefinition(), key)
	third := c.runKeyed(id1, heldDefinition(), key, "restic-repo:other")
	free := c.run(id2, testDefinition())

	// Only the oldest sharer runs; the job without the key does too.
	c.waitStatus(first, domain.JobRunning)
	c.waitStatus(free, domain.JobSucceeded)
	waiting := func(running domain.JobID, queued ...domain.JobID) {
		t.Helper()
		js, _ := c.svc()
		blockers, err := js.Blockers(context.Background(), queued)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range queued {
			if j := c.job(q); j.Status != domain.JobQueued || rn1.runCount(q)+rn2.runCount(q) != 0 || blockers[q] != running {
				t.Fatalf("job %s: %s, %d runs, waits for %v; want it queued behind %s", q, j.Status, rn1.runCount(q)+rn2.runCount(q), blockers[q], running)
			}
		}
	}
	waiting(first, second, third)

	// The controller restarts while the head runs: the order is durable.
	c.restart()
	waiting(first, second, third)

	// Each finished head lets the next one start, on its own worker, in
	// the order they were queued.
	rn1.release(first)
	c.waitStatus(first, domain.JobSucceeded)
	c.waitStatus(second, domain.JobRunning)
	waiting(second, third)

	rn2.release(second)
	c.waitStatus(second, domain.JobSucceeded)
	c.waitStatus(third, domain.JobRunning)
	rn1.release(third)
	c.waitStatus(third, domain.JobSucceeded)

	// Never two at once: each started once the one before it finished
	// (both workers report times from this machine's clock).
	order := []domain.JobID{first, second, third}
	for i := 1; i < len(order); i++ {
		prev, cur := c.job(order[i-1]), c.job(order[i])
		if cur.StartedAt.Before(prev.FinishedAt) {
			t.Errorf("job %d started at %v, before job %d finished at %v", i+1, cur.StartedAt, i, prev.FinishedAt)
		}
	}
	for _, j := range order {
		if runs := rn1.runCount(j) + rn2.runCount(j); runs != 1 {
			t.Errorf("job %s ran %d times", j, runs)
		}
	}
}
