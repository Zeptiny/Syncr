package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// purgeCall is one PurgeJobSamples call the countingSamples saw, with the
// cursor it returned.
type purgeCall struct {
	before      time.Time
	after, next jobs.PurgeCursor
	maxJobs     int
}

// countingSamples records the purge calls before passing them to the
// store; err, if set, fails them instead.
type countingSamples struct {
	jobs.SampleRepository
	mu    sync.Mutex
	calls []purgeCall
	err   error
}

func (c *countingSamples) PurgeJobSamples(ctx context.Context, before time.Time, after jobs.PurgeCursor, maxJobs int) (int, jobs.PurgeCursor, error) {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		c.record(purgeCall{before: before, after: after, maxJobs: maxJobs})
		return 0, after, err
	}
	n, next, err := c.SampleRepository.PurgeJobSamples(ctx, before, after, maxJobs)
	c.record(purgeCall{before: before, after: after, next: next, maxJobs: maxJobs})
	return n, next, err
}

func (c *countingSamples) record(p purgeCall) {
	c.mu.Lock()
	c.calls = append(c.calls, p)
	c.mu.Unlock()
}

func (c *countingSamples) purges() []purgeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]purgeCall(nil), c.calls...)
}

// withSamples returns a service over e's store and clock whose samples go
// through a countingSamples, with cfg.
func (e *env) withSamples(t *testing.T, cfg jobs.Config) (*jobs.Service, *countingSamples) {
	t.Helper()
	cs := &countingSamples{SampleRepository: e.store}
	svc, err := jobs.New(jobs.Deps{
		Jobs: e.store, Samples: cs, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: e.rec, Remotes: e.remotes,
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc, cs
}

// finishedAt makes a job of e that ran and finished at the worker time at,
// with extra samples besides the final one Finish stores.
func (e *env) finishedAt(t *testing.T, at time.Time, extra int) jobs.Job {
	t.Helper()
	ctx := context.Background()
	j := e.create(t)
	if _, _, err := e.svc.MarkRunning(ctx, e.worker, j.ID, epoch, at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := range extra {
		smp := jobs.Sample{JobID: j.ID, Time: at.Add(-time.Duration(i+1) * time.Minute), Stats: jobs.Stats{Bytes: int64(i)}}
		if err := e.store.AppendJobSample(ctx, smp); err != nil {
			t.Fatal(err)
		}
	}
	j, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{
		Status: domain.JobSucceeded, Stats: jobs.Stats{Bytes: 4242}, FinishedAt: at,
	})
	if err != nil || j.Status != domain.JobSucceeded {
		t.Fatalf("finish: %s, %v", j.Status, err)
	}
	return j
}

func (e *env) sampleTotal(t *testing.T, id domain.JobID) int {
	t.Helper()
	_, total, err := e.store.ListJobSamples(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// TestPurgeSamples: PurgeSamples deletes, in batches of PurgeBatchJobs
// jobs, the samples of the jobs that finished before now minus the
// retention, and leaves recent and unfinished jobs' samples, and every
// job, alone.
func TestPurgeSamples(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	svc, cs := e.withSamples(t, jobs.Config{SampleRetention: 2 * time.Hour})
	now := e.clock.Now()

	var old []jobs.Job
	for range jobs.PurgeBatchJobs + 5 {
		old = append(old, e.finishedAt(t, now.Add(-3*time.Hour), 2)) // 3 samples each
	}
	recent := e.finishedAt(t, now.Add(-time.Hour), 4)
	edge := e.finishedAt(t, now.Add(-2*time.Hour), 0) // exactly at the cutoff: kept
	running := e.at(t, domain.JobRunning)
	if err := e.store.AppendJobSample(ctx, jobs.Sample{JobID: running.ID, Time: now.Add(-100 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	n, err := svc.PurgeSamples(ctx)
	if want := 3 * len(old); err != nil || n != want {
		t.Fatalf("PurgeSamples = (%d, %v), want %d", n, err, want)
	}
	calls := cs.purges()
	if len(calls) != 3 { // a full batch, the rest, then nothing left
		t.Errorf("store calls = %+v, want 3", calls)
	}
	for i, c := range calls {
		if !c.before.Equal(now.Add(-2*time.Hour)) || c.maxJobs != jobs.PurgeBatchJobs {
			t.Errorf("store call %+v, want cutoff %s and %d jobs", c, now.Add(-2*time.Hour), jobs.PurgeBatchJobs)
		}
		// Each batch continues where the one before ended, so the jobs
		// already purged are not looked at again.
		if i == 0 && c.after != (jobs.PurgeCursor{}) || i > 0 && c.after != calls[i-1].next {
			t.Errorf("store call %d starts after %+v, want after %+v", i+1, c.after, calls[max(i-1, 0)].next)
		}
	}
	if last := old[len(old)-1]; len(calls) == 3 && calls[1].next != (jobs.PurgeCursor{FinishedAt: last.FinishedAt, ID: last.ID}) {
		t.Errorf("the second batch ended at %+v, want the last old job", calls[1].next)
	}
	for _, j := range old {
		if got := e.sampleTotal(t, j.ID); got != 0 {
			t.Fatalf("old job %s keeps %d samples", j.ID, got)
		}
		// The job and its final counters stay.
		if got, err := e.store.GetJob(ctx, j.ID); err != nil || got.Stats.Bytes != 4242 {
			t.Fatalf("old job after the purge = (%+v, %v)", got, err)
		}
	}
	for name, tt := range map[string]struct {
		id   domain.JobID
		want int
	}{"recent": {recent.ID, 5}, "at the cutoff": {edge.ID, 1}, "running": {running.ID, 1}} {
		if got := e.sampleTotal(t, tt.id); got != tt.want {
			t.Errorf("%s job has %d samples, want %d", name, got, tt.want)
		}
	}

	// Nothing left: one call, nothing deleted.
	if n, err := svc.PurgeSamples(ctx); err != nil || n != 0 {
		t.Errorf("second purge = (%d, %v)", n, err)
	}
	// Time passes: the recent job's samples go too.
	e.clock.Advance(90 * time.Minute)
	if n, err := svc.PurgeSamples(ctx); err != nil || n != 5+1 {
		t.Errorf("purge 90 minutes later = (%d, %v), want the recent and the edge jobs' 6", n, err)
	}
}

// TestPurgeSamplesGoesOnFromTheLastRun: an hourly run starts where the run
// before ended, so the purged history is not walked again; a job whose end
// was reported after the purge passed its (worker clock) end keeps its
// samples until the daily run from the oldest job.
func TestPurgeSamplesGoesOnFromTheLastRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	svc, cs := e.withSamples(t, jobs.Config{SampleRetention: 2 * time.Hour})
	now := e.clock.Now()
	first := e.finishedAt(t, now.Add(-4*time.Hour), 1)
	if n, err := svc.PurgeSamples(ctx); err != nil || n != 2 {
		t.Fatalf("first run = (%d, %v), want the 2 samples of the old job", n, err)
	}
	end := cs.purges()[len(cs.purges())-1].next
	if end.ID != first.ID {
		t.Fatalf("the first run ended at %+v, want the old job", end)
	}

	// An hour later: the run goes on from there.
	e.clock.Advance(time.Hour)
	late := e.finishedAt(t, now.Add(-5*time.Hour), 2) // reported late, with its worker's end time
	before := len(cs.purges())
	if n, err := svc.PurgeSamples(ctx); err != nil || n != 0 {
		t.Errorf("hourly run = (%d, %v), want nothing: the late job lies before where the last run ended", n, err)
	}
	if calls := cs.purges()[before:]; len(calls) != 1 || calls[0].after != end {
		t.Errorf("hourly run calls = %+v, want one from %+v", calls, end)
	}
	if got := e.sampleTotal(t, late.ID); got != 3 {
		t.Errorf("the late job has %d samples, want its 3 until the daily run", got)
	}

	// A day after the first run: from the oldest job again.
	e.clock.Advance(jobs.FullPurgeEvery - time.Hour)
	before = len(cs.purges())
	if n, err := svc.PurgeSamples(ctx); err != nil || n != 3 {
		t.Errorf("daily run = (%d, %v), want the late job's 3 samples", n, err)
	}
	if calls := cs.purges()[before:]; len(calls) == 0 || calls[0].after != (jobs.PurgeCursor{}) {
		t.Errorf("daily run calls = %+v, want the first from the start", calls)
	}
}

// TestPurgeSamplesConfig: the zero Config keeps 30 days; KeepSamples
// deletes nothing and never asks the store; a retention under the minimum
// is refused.
func TestPurgeSamplesConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	now := e.clock.Now()
	month := e.finishedAt(t, now.Add(-jobs.DefaultSampleRetention+time.Minute), 0)
	older := e.finishedAt(t, now.Add(-jobs.DefaultSampleRetention-time.Minute), 0)

	keep, cs := e.withSamples(t, jobs.Config{KeepSamples: true, SampleRetention: time.Minute})
	if n, err := keep.PurgeSamples(ctx); err != nil || n != 0 || len(cs.purges()) != 0 {
		t.Errorf("KeepSamples: PurgeSamples = (%d, %v) after %d store calls", n, err, len(cs.purges()))
	}
	def, _ := e.withSamples(t, jobs.Config{})
	if n, err := def.PurgeSamples(ctx); err != nil || n != 1 {
		t.Errorf("default retention: PurgeSamples = (%d, %v), want 1", n, err)
	}
	if e.sampleTotal(t, month.ID) != 1 || e.sampleTotal(t, older.ID) != 0 {
		t.Error("the default retention is not 30 days")
	}
	if jobs.DefaultConfig().SampleRetention != 720*time.Hour || jobs.MinSampleRetention != time.Hour {
		t.Errorf("defaults = %v, minimum %v", jobs.DefaultConfig().SampleRetention, jobs.MinSampleRetention)
	}

	for _, bad := range []time.Duration{time.Hour - time.Second, -time.Hour} {
		_, err := jobs.New(jobs.Deps{
			Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: &idgen.Sequential{},
			Broker: broker.New(), Notifier: jobs.NopNotifier{}, Waker: e.rec,
		}, jobs.Config{SampleRetention: bad})
		if err == nil {
			t.Errorf("a sample retention of %v was accepted", bad)
		}
	}
}

// TestPurgeSamplesErrors: a store failure is returned; a context that
// ended stops the purge before the next batch.
func TestPurgeSamplesErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	svc, cs := e.withSamples(t, jobs.Config{SampleRetention: time.Hour})
	cs.err = errors.New("db down")
	if _, err := svc.PurgeSamples(context.Background()); err == nil || !errors.Is(err, cs.err) {
		t.Errorf("store failure: err = %v", err)
	}
	cs.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(cs.purges())
	if _, err := svc.PurgeSamples(ctx); !errors.Is(err, context.Canceled) || len(cs.purges()) != before {
		t.Errorf("cancelled: err = %v after %d store calls", err, len(cs.purges())-before)
	}
}

func TestEffectiveTimeLimit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		maxRuntime, def time.Duration
		systemOnly      bool
		want            time.Duration
	}{
		{"none", 0, 0, false, 0},
		{"the definition's", 6 * time.Hour, 0, false, 6 * time.Hour},
		{"the definition's over the default", time.Hour, 24 * time.Hour, false, time.Hour},
		{"the default", 0, 24 * time.Hour, false, 24 * time.Hour},
		{"a system kind never gets the default", 0, 24 * time.Hour, true, 0},
		{"a system kind keeps its own", 2 * time.Hour, 24 * time.Hour, true, 2 * time.Hour},
		{"a negative definition value is unset", -time.Hour, 3 * time.Hour, false, 3 * time.Hour},
		{"a negative default is none", 0, -time.Hour, false, 0},
	} {
		if got := jobs.EffectiveTimeLimit(tt.maxRuntime, tt.def, tt.systemOnly); got != tt.want {
			t.Errorf("%s: EffectiveTimeLimit = %v, want %v", tt.name, got, tt.want)
		}
	}
}
