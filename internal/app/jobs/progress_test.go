package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// countingJobs counts the job reads of the service.
type countingJobs struct {
	jobs.Repository
	gets atomic.Int64
}

func (c *countingJobs) GetJob(ctx context.Context, id domain.JobID) (jobs.Job, error) {
	c.gets.Add(1)
	return c.Repository.GetJob(ctx, id)
}

// progressEnv is an env whose service reads jobs through a countingJobs
// and publishes to a broker nobody subscribed to.
func progressEnv(t testing.TB) (*env, *countingJobs) {
	t.Helper()
	e := newEnv(t)
	repo := &countingJobs{Repository: e.store}
	svc, err := jobs.New(jobs.Deps{
		Jobs: repo, Samples: e.store, Clock: e.clock, IDs: &idgen.Sequential{},
		Broker: broker.New(), Notifier: e.rec, Waker: e.rec, Remotes: e.remotes,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e, repo
}

// BenchmarkRecordProgress is the ingest of one progress event of a running
// job, at the workers' default rate of one event a second (the clock and
// the sample times advance a second per event). job-reads/op is the number
// of Repository.GetJob calls per event; each is two statements on the
// Postgres store.
func BenchmarkRecordProgress(b *testing.B) {
	ctx := context.Background()
	e, repo := progressEnv(b)
	j := e.at(b, domain.JobRunning)
	repo.gets.Store(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		e.clock.Advance(time.Second)
		smp := jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: int64(i)}}
		if ok, err := e.svc.RecordProgress(ctx, e.worker, j.ID, epoch, smp); !ok || err != nil {
			b.Fatalf("RecordProgress = (%v, %v)", ok, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(repo.gets.Load())/float64(b.N), "job-reads/op")
}

// TestProgressReadsTheJobOncePerStoredSample: between two stored samples
// the job is not read again, unless the sample comes from another worker
// or epoch, or the last read is older than the sample interval; the end of
// the job is noticed at once.
func TestProgressReadsTheJobOncePerStoredSample(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, repo := progressEnv(t)
	j := e.at(t, domain.JobRunning)
	t0 := e.clock.Now()
	record := func(worker domain.WorkerID, ep string, at time.Duration) (bool, error) {
		return e.svc.RecordProgress(ctx, worker, j.ID, ep, jobs.Sample{Time: t0.Add(at)})
	}
	reads := func() int64 { return repo.gets.Swap(0) }
	reads()

	// One read for the stored sample, none for the four after it.
	for sec := range 5 {
		if ok, err := record(e.worker, epoch, time.Duration(sec)*time.Second); !ok || err != nil {
			t.Fatalf("RecordProgress(%ds) = (%v, %v)", sec, ok, err)
		}
	}
	if n := reads(); n != 1 {
		t.Errorf("job reads for five samples within the interval = %d, want 1", n)
	}
	// Another epoch or worker is checked against the job.
	if ok, err := record(e.worker, "other", time.Second); ok || err != nil {
		t.Errorf("progress of another epoch = (%v, %v)", ok, err)
	}
	if _, err := record(e.addWorker(t), epoch, time.Second); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("progress of another worker: err = %v", err)
	}
	if n := reads(); n != 2 {
		t.Errorf("job reads for another epoch and worker = %d, want 2", n)
	}
	// The sample that is due is read and stored; so is one whose last
	// read is too old on the controller's clock, though it is not stored.
	if ok, err := record(e.worker, epoch, 5*time.Second); !ok || err != nil {
		t.Fatalf("RecordProgress(5s) = (%v, %v)", ok, err)
	}
	e.clock.Advance(jobs.DefaultConfig().SampleInterval)
	if ok, err := record(e.worker, epoch, 6*time.Second); !ok || err != nil {
		t.Fatalf("RecordProgress(6s) = (%v, %v)", ok, err)
	}
	if ok, err := record(e.worker, epoch, 7*time.Second); !ok || err != nil {
		t.Fatalf("RecordProgress(7s) = (%v, %v)", ok, err)
	}
	if n := reads(); n != 2 {
		t.Errorf("job reads for a due sample and a stale check = %d, want 2", n)
	}
	if stored, _ := e.svc.Samples(ctx, e.owner, j.ID); len(stored) != 2 {
		t.Errorf("stored %d samples, want 2 (0 s and 5 s)", len(stored))
	}
	// The job's end is noticed by the next sample.
	if _, _, err := e.svc.Finish(ctx, e.worker, j.ID, epoch, jobs.Outcome{Status: domain.JobSucceeded}); err != nil {
		t.Fatal(err)
	}
	if ok, err := record(e.worker, epoch, 8*time.Second); ok || err != nil {
		t.Errorf("progress of a finished job = (%v, %v)", ok, err)
	}
}
