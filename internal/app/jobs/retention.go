package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Limits of the sample retention (Config.SampleRetention,
// jobs.sample_retention).
const (
	// DefaultSampleRetention keeps the samples of a finished job for 30
	// days.
	DefaultSampleRetention = 30 * 24 * time.Hour
	// MinSampleRetention is the shortest retention.
	MinSampleRetention = time.Hour
	// PurgeBatchJobs is how many jobs' samples one store call of
	// PurgeSamples deletes, so no single statement deletes too much.
	PurgeBatchJobs = 50
	// FullPurgeEvery is how often PurgeSamples walks the finished jobs from
	// the oldest one again instead of going on from where its last run
	// ended.
	FullPurgeEvery = 24 * time.Hour
)

// purgeState is where the sample purge goes on from (Service.PurgeSamples).
type purgeState struct {
	// mu serializes the runs.
	mu sync.Mutex
	// from is the cursor the last run ended at; fullAt is when a run last
	// started from the oldest job (zero: never).
	from   PurgeCursor
	fullAt time.Time
}

// PurgeSamples deletes the progress samples of the jobs that finished more
// than Config.SampleRetention ago (by the injected clock), in batches of
// PurgeBatchJobs jobs until none is left, each batch continuing after the
// last job of the one before (PurgeCursor), and returns how many samples it
// deleted. Samples of unfinished jobs are never deleted, nor are the jobs
// themselves: their final counters stay on the job. With
// Config.KeepSamples it deletes nothing. When ctx ends between two
// batches it returns what it deleted so far with ctx's error.
//
// A run goes on from the cursor the previous one ended at, so the hourly
// purge does not walk the whole history of finished jobs again each time.
// The jobs before that cursor have had their samples purged, except one
// whose end a worker reported after the purge had passed its (worker
// clock) FinishedAt: so every FullPurgeEvery, and at the first run, a run
// starts from the oldest job again.
func (s *Service) PurgeSamples(ctx context.Context) (int, error) {
	if s.cfg.KeepSamples {
		return 0, nil
	}
	p := &s.purge
	p.mu.Lock()
	defer p.mu.Unlock()
	now := s.clock.Now()
	cutoff := now.Add(-s.cfg.SampleRetention)
	if p.fullAt.IsZero() || now.Sub(p.fullAt) >= FullPurgeEvery {
		p.from, p.fullAt = PurgeCursor{}, now
	}
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("purge job samples: %w", err)
		}
		n, next, err := s.samples.PurgeJobSamples(ctx, cutoff, p.from, PurgeBatchJobs)
		if err != nil {
			return total, fmt.Errorf("purge job samples finished before %s: %w", cutoff.Format(time.RFC3339), err)
		}
		if n <= 0 {
			return total, nil
		}
		total += n
		p.from = next
	}
}
