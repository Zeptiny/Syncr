package probe

import (
	"context"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Transport runs a probe on a worker and returns its result
// (*dispatch.ProbeBroker; the dev seed's canned fake). A result with
// status FAILED or CANCELLED is a result. Errors: domain.ErrNotFound (no
// such worker), domain.ErrUnavailable (offline, no answer in time, the
// worker restarted, too many probes in flight),
// domain.ErrFailedPrecondition (the worker cannot run the operation), and
// ctx's errors.
type Transport interface {
	Probe(ctx context.Context, workerID domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error)
}

// Workers is the part of the worker use cases probes use
// (*workers.Service).
type Workers interface {
	// Get returns a worker; ErrNotFound if it does not exist.
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
	// List returns the workers matching f (removed ones only with
	// f.IncludeRemoved).
	List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error)
	// Status derives a worker's connection status now.
	Status(w workers.Worker) domain.WorkerStatus
}

// Jobs is the part of the job use cases the worker pick uses
// (*jobs.Service).
type Jobs interface {
	// Unfinished returns a worker's queued, dispatched and running jobs.
	Unfinished(ctx context.Context, workerID domain.WorkerID) ([]jobs.Job, error)
	// List returns the jobs the actor may see that match f, newest first.
	List(ctx context.Context, actor domain.Actor, f jobs.ListFilter) (jobs.Page, error)
}

// Remotes resolves the remotes of a probe with their credentials
// (*remotes.Service).
type Remotes interface {
	// Resolve returns ids and the members of the unions among them, keyed
	// by ID. ErrNotFound if any is missing, deleted or not owner's;
	// ErrFailedPrecondition if stored secrets cannot be decrypted.
	Resolve(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Resolved, error)
	// Draft resolves an unsaved remote: in validated like Create (id zero)
	// or merged over the actor's stored remote id like Update, without
	// storing anything. It returns the remote and the members of a union,
	// and the ID the draft is keyed by (a fresh one for a new remote).
	Draft(ctx context.Context, actor domain.Actor, id domain.RemoteID, in remotes.Input) (map[domain.RemoteID]remotes.Resolved, domain.RemoteID, error)
}

// Limiter decides whether a user may run another probe
// (*ratelimit.Limiter, keyed by user ID).
type Limiter interface {
	Allow(key string) bool
}

// IDGenerator mints probe IDs.
type IDGenerator interface {
	NewProbeID() domain.ProbeID
}
