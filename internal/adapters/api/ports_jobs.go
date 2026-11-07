package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// JobService is the part of the jobs use cases the API calls (implemented
// by *jobs.Service).
//
// m3: pending track C: the use cases gain the actor (owner scoping, admins
// seeing every job), remote references and search; RunJob checks the
// definition with Deps.Kinds before Create.
type JobService interface {
	Create(ctx context.Context, p jobs.CreateParams) (jobs.Job, error)
	Get(ctx context.Context, id domain.JobID) (jobs.Job, error)
	List(ctx context.Context, f jobs.ListFilter) (jobs.Page, error)
	Cancel(ctx context.Context, id domain.JobID) (jobs.Job, error)
	LatestSample(ctx context.Context, id domain.JobID) (jobs.Sample, bool, error)
	Watch(ctx context.Context, id domain.JobID, fn func(jobs.Update) error) error
}

var _ JobService = (*jobs.Service)(nil)
