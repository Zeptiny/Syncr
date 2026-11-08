package dispatch

import (
	"context"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Jobs is the part of the job service (*jobs.Service) the dispatcher uses.
// Its lifecycle methods are idempotent and safe for concurrent use (see
// package jobs).
type Jobs interface {
	Unfinished(ctx context.Context, workerID domain.WorkerID) ([]jobs.Job, error)
	QueuedBefore(ctx context.Context, before time.Time, limit int) ([]jobs.Job, error)
	MarkDispatched(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string) (jobs.Job, bool, error)
	MarkRunning(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, startedAt time.Time) (jobs.Job, bool, error)
	RecordProgress(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, smp jobs.Sample) (bool, error)
	Finish(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, o jobs.Outcome) (jobs.Job, bool, error)
	Requeue(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string) (jobs.Job, bool, error)
	Interrupt(ctx context.Context, workerID domain.WorkerID, id domain.JobID, epoch string, e jobs.JobError) (jobs.Job, bool, error)
	FailQueued(ctx context.Context, id domain.JobID, queuedBefore time.Time, e jobs.JobError) (jobs.Job, bool, error)
}

// SpecBuilder builds the worker spec of a queued job from its stored
// definition, resolving the credentials of the remotes it uses. It is
// called for every queued job on every poll until the worker accepts it, so
// the spec (which contains secrets) is never stored. Specs (spec.go) is the
// implementation over the remotes service and the job kinds.
//
// Errors that fail the job instead of being retried: domain.ErrNotFound (a
// remote was deleted or is not the owner's) fails it with
// "remote_unavailable", domain.ErrInvalidArgument (the definition cannot be
// decoded or built) with "invalid_spec", domain.ErrFailedPrecondition (a
// remote's stored credentials cannot be used, e.g. they no longer decrypt)
// with "internal". Any other error is returned to the worker's call and
// retried on its next poll.
type SpecBuilder interface {
	Build(ctx context.Context, j jobs.Job) (*syncrv1.JobSpec, error)
}

// Remotes resolves a job's remotes with their credentials
// (*remotes.Service).
type Remotes interface {
	// Resolve returns ids and the members of the unions among them,
	// decrypted and mapped to rclone remotes, keyed by ID. ErrNotFound if
	// any is missing, deleted or not owner's; ErrFailedPrecondition if a
	// remote's stored secrets cannot be decrypted.
	Resolve(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Resolved, error)
}

// Workers is the part of the worker service (*workers.Service) the
// dispatcher uses.
type Workers interface {
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
	AdvanceEpoch(ctx context.Context, w workers.Worker, epoch string) (workers.Worker, error)
	AdvanceAckSeq(ctx context.Context, id domain.WorkerID, epoch string, seq int64) (int64, error)
	SetInfo(ctx context.Context, id domain.WorkerID, epoch string, info workers.Info) error
	Touch(ctx context.Context, id domain.WorkerID) error
	Status(w workers.Worker) domain.WorkerStatus
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// PassiveClient calls one passive worker's WorkerLinkService. The passive
// transport implements it (a Connect client with the worker's pinned
// certificate and secret); it must return an error for any transport or
// RPC failure and may be called from one goroutine at a time.
type PassiveClient interface {
	Command(ctx context.Context, req *syncrv1.CommandRequest) (*syncrv1.CommandResponse, error)
	Pull(ctx context.Context, req *syncrv1.PullRequest) (*syncrv1.PullResponse, error)
}
