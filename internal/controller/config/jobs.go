package config

import (
	"errors"
	"fmt"
	"time"
)

// Jobs configures job time limits (internal/controller/dispatch, which
// sends a job's limit to its worker) and how long the progress samples of
// finished jobs are kept (internal/app/jobs).
type Jobs struct {
	// DefaultMaxRuntime is the time limit of the jobs whose definition sets
	// none (max_runtime): the worker stops such a job after this long and it
	// ends failed (time_limit). 0, the default, means no limit; otherwise
	// between MinJobTimeLimit and MaxJobTimeLimit. Jobs of kinds that only
	// the controller starts (a repository's data deletion) never get it.
	DefaultMaxRuntime time.Duration `yaml:"default_max_runtime" env:"SYNCR_JOBS_DEFAULT_MAX_RUNTIME"`
	// SampleRetention is how long the progress samples (the chart) of a
	// finished job are kept, counted from its end (default 720h, 30 days;
	// at least MinSampleRetention). 0 keeps them forever. The job itself and
	// its final counters are always kept.
	SampleRetention time.Duration `yaml:"sample_retention" env:"SYNCR_JOBS_SAMPLE_RETENTION"`
}

// Limits of the jobs settings. They equal the bounds of internal/app/jobs
// (MinTimeLimit, MaxTimeLimit, DefaultSampleRetention and
// MinSampleRetention); a test in cmd/syncr-controller keeps them in step.
const (
	MinJobTimeLimit        = time.Minute
	MaxJobTimeLimit        = 366 * 24 * time.Hour
	DefaultSampleRetention = 30 * 24 * time.Hour
	MinSampleRetention     = time.Hour
)

// KeepSamples reports whether job samples are never purged
// (jobs.sample_retention: 0).
func (j Jobs) KeepSamples() bool { return j.SampleRetention == 0 }

func defaultJobs() Jobs {
	return Jobs{SampleRetention: DefaultSampleRetention}
}

func (j Jobs) validate() error {
	var errs []error
	if d := j.DefaultMaxRuntime; d != 0 && (d < MinJobTimeLimit || d > MaxJobTimeLimit) {
		errs = append(errs, fmt.Errorf("jobs.default_max_runtime must be 0 (no limit) or between %v and %v (366 days)", MinJobTimeLimit, MaxJobTimeLimit))
	}
	if r := j.SampleRetention; r != 0 && r < MinSampleRetention {
		errs = append(errs, fmt.Errorf("jobs.sample_retention must be 0 (keep every sample) or at least %v", MinSampleRetention))
	}
	return errors.Join(errs...)
}
