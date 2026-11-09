package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// JobService is the part of the jobs use cases the API calls (implemented
// by *jobs.Service). Every use case but LatestSamples takes the caller's
// actor and scopes by it: a user's own jobs, any job for an admin, and
// domain.ErrNotFound for another user's job.
type JobService interface {
	Create(ctx context.Context, actor domain.Actor, p jobs.CreateParams) (jobs.Job, error)
	Get(ctx context.Context, actor domain.Actor, id domain.JobID) (jobs.Job, error)
	List(ctx context.Context, actor domain.Actor, f jobs.ListFilter) (jobs.Page, error)
	Cancel(ctx context.Context, actor domain.Actor, id domain.JobID) (jobs.Job, error)
	Watch(ctx context.Context, actor domain.Actor, id domain.JobID, fn func(jobs.Update) error) error
	Samples(ctx context.Context, actor domain.Actor, id domain.JobID) ([]jobs.Sample, error)
	// LatestSamples is only called for jobs the actor has just read; one
	// store query for all of them.
	LatestSamples(ctx context.Context, ids []domain.JobID) (map[domain.JobID]jobs.Sample, error)
}

var _ JobService = (*jobs.Service)(nil)

// RemoteNames looks up the display references of jobs' remotes, of any
// owner and deleted ones included (RemoteService.Names).
type RemoteNames interface {
	Names(ctx context.Context, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Ref, error)
}
