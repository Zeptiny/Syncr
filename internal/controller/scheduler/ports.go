package scheduler

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Schedules is the scheduler-facing part of the schedules use cases
// (*schedules.Service).
type Schedules interface {
	FireDue(ctx context.Context) (int, error)
	OpenRuns(ctx context.Context, limit int) ([]schedules.Run, error)
	Load(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]schedules.Schedule, error)
	HasActiveRun(ctx context.Context, id domain.ScheduleID) (bool, error)
	MarkLaunching(ctx context.Context, run schedules.Run, jobID domain.JobID, workerID domain.WorkerID) (schedules.Run, error)
	MarkLaunched(ctx context.Context, run schedules.Run) (schedules.Run, error)
	EndRun(ctx context.Context, run schedules.Run, status schedules.RunStatus, reason, msg string, pause bool) (schedules.Run, error)
}

// Jobs is the part of the job use cases the scheduler uses (*jobs.Service).
type Jobs interface {
	// Create queues a job for the actor; a preset p.ID makes it idempotent
	// (ErrAlreadyExists once it exists).
	Create(ctx context.Context, actor domain.Actor, p jobs.CreateParams) (jobs.Job, error)
	// Unfinished returns a worker's queued, dispatched and running jobs.
	Unfinished(ctx context.Context, workerID domain.WorkerID) ([]jobs.Job, error)
}

// Workers is the part of the worker use cases the scheduler uses
// (*workers.Service).
type Workers interface {
	// Get returns a worker that is not removed; ErrNotFound otherwise.
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
	// List returns the workers matching f (removed ones only with
	// f.IncludeRemoved).
	List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error)
	// Status derives a worker's connection status now.
	Status(w workers.Worker) domain.WorkerStatus
}

// Owners tells whether a schedule's owner may still run jobs
// (*users.Service).
type Owners interface {
	// Active reports whether the user exists and is not disabled.
	Active(ctx context.Context, id domain.UserID) (bool, error)
}

// IDGenerator mints the job IDs a launch reserves (UUIDv7).
type IDGenerator interface {
	NewJobID() domain.JobID
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}
