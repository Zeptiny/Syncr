package api

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// WorkerService is the part of the workers use cases the API calls
// (implemented by *workers.Service).
type WorkerService interface {
	CreateEnrollmentToken(ctx context.Context, ttl time.Duration) (workers.EnrollmentGrant, error)
	RegisterPassive(ctx context.Context, p workers.PassiveParams) (workers.Worker, error)
	List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error)
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
	Rename(ctx context.Context, id domain.WorkerID, name string) (workers.Worker, error)
	Remove(ctx context.Context, id domain.WorkerID) error
	// Status derives a worker's connection status as of now.
	Status(w workers.Worker) domain.WorkerStatus
}

// JobService is the part of the jobs use cases the API calls (implemented by
// *jobs.Service).
type JobService interface {
	Create(ctx context.Context, p jobs.CreateParams) (jobs.Job, error)
	Get(ctx context.Context, id domain.JobID) (jobs.Job, error)
	List(ctx context.Context, f jobs.ListFilter) (jobs.Page, error)
	Cancel(ctx context.Context, id domain.JobID) (jobs.Job, error)
	LatestSample(ctx context.Context, id domain.JobID) (jobs.Sample, bool, error)
	Watch(ctx context.Context, id domain.JobID, fn func(jobs.Update) error) error
}

var (
	_ WorkerService = (*workers.Service)(nil)
	_ JobService    = (*jobs.Service)(nil)
)
