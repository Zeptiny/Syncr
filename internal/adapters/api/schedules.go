package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// scheduleHandler implements syncr.v1.ScheduleService over the schedules
// use cases (Deps.Schedules), checking job definitions with Deps.Kinds as
// RunJob does. The contracts step mounts it with every procedure
// Unimplemented; the schedules track fills it in.
type scheduleHandler struct {
	syncrv1connect.UnimplementedScheduleServiceHandler

	schedules ScheduleService
	kinds     *jobkind.Registry
	remotes   RemoteService
	contacts  ContactService
	errs      errorMapper
}

var _ syncrv1connect.ScheduleServiceHandler = (*scheduleHandler)(nil)
