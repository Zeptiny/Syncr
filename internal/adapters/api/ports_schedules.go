package api

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// ScheduleService is the part of the schedules use cases the API calls
// (implemented by *schedules.Service; see internal/app/schedules for the
// semantics). Every use case taking an actor scopes by it: the actor's own
// schedules only, admins included, and domain.ErrNotFound otherwise.
// ScheduleService, DashboardService and the jobs' schedule references use
// it.
type ScheduleService interface {
	Create(ctx context.Context, actor domain.Actor, in schedules.Input) (schedules.Schedule, error)
	Get(ctx context.Context, actor domain.Actor, id domain.ScheduleID) (schedules.Schedule, error)
	List(ctx context.Context, actor domain.Actor, f schedules.ListFilter) ([]schedules.Schedule, error)
	Update(ctx context.Context, actor domain.Actor, id domain.ScheduleID, version int64, in schedules.Input) (schedules.Schedule, error)
	SetEnabled(ctx context.Context, actor domain.Actor, id domain.ScheduleID, enabled bool) (schedules.Schedule, error)
	Delete(ctx context.Context, actor domain.Actor, id domain.ScheduleID) error
	RunNow(ctx context.Context, actor domain.Actor, id domain.ScheduleID) (schedules.Run, error)
	Runs(ctx context.Context, actor domain.Actor, id domain.ScheduleID, before domain.ScheduleRunID, limit int) (schedules.RunPage, error)
	// LatestRuns returns the newest run of each of the actor's schedules
	// ids (Schedule.last_run).
	LatestRuns(ctx context.Context, actor domain.Actor, ids []domain.ScheduleID) (map[domain.ScheduleID]schedules.Run, error)
	// Preview checks an expression (PreviewSchedule); it needs no actor.
	Preview(cron, timezone string, after time.Time, count int) (schedules.Preview, error)
	// Names returns display references of any schedules (jobs' schedules).
	Names(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]schedules.Ref, error)

	// The dashboard's parts: the actor's next occurrences, latest missed or
	// failed runs, and missed runs scheduled in [from, until).
	Upcoming(ctx context.Context, actor domain.Actor, limit int) ([]schedules.Upcoming, error)
	RecentProblems(ctx context.Context, actor domain.Actor, limit int) ([]schedules.Run, error)
	CountMissed(ctx context.Context, actor domain.Actor, from, until time.Time) (int64, error)
}

var _ ScheduleService = (*schedules.Service)(nil)

// ScheduleNames looks up the display references of jobs' schedules, of any
// owner (ScheduleService.Names).
type ScheduleNames interface {
	Names(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]schedules.Ref, error)
}

// ContactNames looks up the display references of schedules' contacts
// (ContactService.Names).
type ContactNames interface {
	Names(ctx context.Context, ids []domain.ContactID) (map[domain.ContactID]contacts.Ref, error)
}
