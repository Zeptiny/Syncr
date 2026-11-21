package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// DashboardJobs is the part of the jobs use cases only the dashboard and
// the metrics call (implemented by *jobs.Service in jobs/activity.go).
// DashboardService and GET /metrics are mounted when Deps.Jobs also
// implements it and Deps.Workers and Deps.Schedules are set. They read
// recent failures through JobService.List, the workers through
// WorkerService and the schedules' parts through ScheduleService.
type DashboardJobs interface {
	// Activity counts the finished jobs of f by bucket, without gaps: the
	// actor's, or every user's for an admin with f.AllUsers.
	Activity(ctx context.Context, actor domain.Actor, f jobs.ActivityFilter) ([]jobs.ActivityBucket, error)
	// CountUnfinished counts the queued, dispatched and running jobs the
	// same way, by status.
	CountUnfinished(ctx context.Context, actor domain.Actor, allUsers bool) (map[domain.JobStatus]int64, error)
	// UnfinishedCounts counts them by status, kind and worker, with the
	// oldest QueuedAt of each group.
	UnfinishedCounts(ctx context.Context, actor domain.Actor, allUsers bool) ([]jobs.UnfinishedRow, error)
	// Totals counts the finished jobs of f by kind, worker, status, error
	// code and run-time bucket (every finished job with zero bounds).
	Totals(ctx context.Context, actor domain.Actor, f jobs.TotalsFilter) ([]jobs.TotalsRow, error)
}

var _ DashboardJobs = (*jobs.Service)(nil)
