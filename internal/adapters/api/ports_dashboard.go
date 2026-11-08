package api

import (
	"context"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// DashboardJobs is the part of the jobs use cases only the dashboard calls
// (implemented by *jobs.Service in jobs/activity.go). DashboardService
// is mounted when Deps.Jobs also implements it and Deps.Workers and
// Deps.Schedules are set. It reads recent failures through JobService.List,
// the workers through WorkerService and the schedules' parts through
// ScheduleService.
type DashboardJobs interface {
	// Activity counts the finished jobs of f by bucket, without gaps: the
	// actor's, or every user's for an admin with f.AllUsers.
	Activity(ctx context.Context, actor domain.Actor, f jobs.ActivityFilter) ([]jobs.ActivityBucket, error)
	// CountUnfinished counts the queued, dispatched and running jobs the
	// same way, by status.
	CountUnfinished(ctx context.Context, actor domain.Actor, allUsers bool) (map[domain.JobStatus]int64, error)
}
