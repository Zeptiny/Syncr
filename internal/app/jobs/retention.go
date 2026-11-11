package jobs

import (
	"context"
	"fmt"
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
)

// PurgeSamples deletes the progress samples of the jobs that finished more
// than Config.SampleRetention ago (by the injected clock), in batches of
// PurgeBatchJobs jobs until none is left, and returns how many samples it
// deleted. Samples of unfinished jobs are never deleted, nor are the jobs
// themselves: their final counters stay on the job. With
// Config.KeepSamples it deletes nothing. When ctx ends between two
// batches it returns what it deleted so far with ctx's error.
func (s *Service) PurgeSamples(ctx context.Context) (int, error) {
	if s.cfg.KeepSamples {
		return 0, nil
	}
	cutoff := s.clock.Now().Add(-s.cfg.SampleRetention)
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("purge job samples: %w", err)
		}
		n, err := s.samples.PurgeJobSamples(ctx, cutoff, PurgeBatchJobs)
		if err != nil {
			return total, fmt.Errorf("purge job samples finished before %s: %w", cutoff.Format(time.RFC3339), err)
		}
		if n <= 0 {
			return total, nil
		}
		total += n
	}
}
