package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// dashboardHandler implements syncr.v1.DashboardService, composed from the
// jobs, workers and schedules use cases (no use case of its own). The
// contracts step mounts it Unimplemented; the dashboard track fills it in.
type dashboardHandler struct {
	syncrv1connect.UnimplementedDashboardServiceHandler

	jobs      JobService
	activity  DashboardJobs
	workers   WorkerService
	schedules ScheduleService
	errs      errorMapper
}

var _ syncrv1connect.DashboardServiceHandler = (*dashboardHandler)(nil)
