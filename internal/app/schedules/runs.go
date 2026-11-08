package schedules

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/cronexpr"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Dashboard limits.
const (
	// DefaultUpcoming and DefaultProblems are the dashboard's list sizes
	// (limit 0); MaxDashboardList bounds them.
	DefaultUpcoming  = 8
	DefaultProblems  = 5
	MaxDashboardList = 100
)

// RunNow adds a manual run of one of actor's schedules, deadline counted
// from now (ManualDeadline), on the same terms as a cron run: it waits for
// its worker and for an active run (overlap queue), and merges into the
// open run if there is one (the result is then that run). A paused
// schedule may run now too. ErrFailedPrecondition if the overlap policy is
// skip and a run of the schedule is active.
func (s *Service) RunNow(ctx context.Context, actor domain.Actor, id domain.ScheduleID) (Run, error) {
	sc, err := s.owned(ctx, actor, id)
	if err != nil {
		return Run{}, fmt.Errorf("run schedule now: %w", err)
	}
	e, err := cronexpr.Parse(sc.Cron, sc.Timezone)
	if err != nil {
		return Run{}, fmt.Errorf("run schedule %s now: %w: %w", id, domain.ErrFailedPrecondition, err)
	}
	if sc.Overlap.orDefault() == OverlapSkip {
		active, err := s.repo.HasActiveRun(ctx, id)
		if err != nil {
			return Run{}, fmt.Errorf("run schedule %s now: %w", id, err)
		}
		if active {
			return Run{}, fmt.Errorf("run schedule %s now: %w: a run of the schedule is active and its overlap policy is skip",
				id, domain.ErrFailedPrecondition)
		}
	}
	now := s.clock.Now()
	r := Run{
		ID: s.ids.NewScheduleRunID(), ScheduleID: id, Trigger: TriggerManual,
		ScheduledFor: now, Deadline: ManualDeadline(e, sc.Missed, now),
		Status: RunPending, CreatedAt: now, UpdatedAt: now,
	}
	// An open run created between AddRun's merge and its insert makes the
	// insert fail; the next attempt merges into it.
	for attempt := 0; ; attempt++ {
		stored, err := s.repo.AddRun(ctx, r, true)
		if errors.Is(err, domain.ErrAlreadyExists) && attempt < 2 {
			continue
		}
		if err != nil {
			return Run{}, fmt.Errorf("run schedule %s now: %w", id, err)
		}
		s.log.InfoContext(ctx, "schedule run requested", "schedule_id", id.String(), "run_id", stored.ID.String(),
			"actor_id", actor.UserID.String(), "merged", stored.ID != r.ID)
		s.waker.Wake()
		return stored, nil
	}
}

// Runs returns a page of one of actor's schedules' runs, newest first:
// before is the cursor (zero: the newest), limit 0 means DefaultPageSize,
// at most MaxPageSize.
func (s *Service) Runs(ctx context.Context, actor domain.Actor, id domain.ScheduleID, before domain.ScheduleRunID, limit int) (RunPage, error) {
	switch {
	case limit == 0:
		limit = DefaultPageSize
	case limit < 0 || limit > MaxPageSize:
		return RunPage{}, fmt.Errorf("list runs: %w: limit %d outside [1, %d]", domain.ErrInvalidArgument, limit, MaxPageSize)
	}
	if _, err := s.owned(ctx, actor, id); err != nil {
		return RunPage{}, fmt.Errorf("list runs: %w", err)
	}
	rs, err := s.repo.ListRuns(ctx, RunFilter{ScheduleID: id, Before: before, Limit: limit + 1})
	if err != nil {
		return RunPage{}, fmt.Errorf("list runs of schedule %s: %w", id, err)
	}
	var p RunPage
	if len(rs) > limit {
		rs = rs[:limit]
		p.Next = rs[limit-1].ID
	}
	p.Runs = rs
	return p, nil
}

// LatestRuns returns the newest run of each of actor's schedules among ids
// that has one; other users' schedules are left out.
func (s *Service) LatestRuns(ctx context.Context, actor domain.Actor, ids []domain.ScheduleID) (map[domain.ScheduleID]Run, error) {
	out := map[domain.ScheduleID]Run{}
	if len(ids) == 0 || actor.UserID.IsZero() {
		return out, nil
	}
	ss, err := s.repo.GetSchedules(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("latest runs: %w", err)
	}
	mine := make([]domain.ScheduleID, 0, len(ss))
	for _, sc := range ss {
		if actor.Owns(sc.OwnerID) {
			mine = append(mine, sc.ID)
		}
	}
	if len(mine) == 0 {
		return out, nil
	}
	latest, err := s.repo.LatestRuns(ctx, mine)
	if err != nil {
		return nil, fmt.Errorf("latest runs: %w", err)
	}
	return latest, nil
}

// Upcoming returns the next occurrences of actor's enabled schedules,
// soonest first (limit 0 means DefaultUpcoming, at most MaxDashboardList).
func (s *Service) Upcoming(ctx context.Context, actor domain.Actor, limit int) ([]Upcoming, error) {
	if actor.UserID.IsZero() {
		return []Upcoming{}, nil
	}
	out, err := s.repo.ListUpcoming(ctx, actor.UserID, dashboardLimit(limit, DefaultUpcoming))
	if err != nil {
		return nil, fmt.Errorf("upcoming runs: %w", err)
	}
	return out, nil
}

// RecentProblems returns the latest missed and failed runs of actor's
// schedules, newest first (limit 0 means DefaultProblems, at most
// MaxDashboardList).
func (s *Service) RecentProblems(ctx context.Context, actor domain.Actor, limit int) ([]Run, error) {
	if actor.UserID.IsZero() {
		return []Run{}, nil
	}
	out, err := s.repo.ListRuns(ctx, RunFilter{
		OwnerID: actor.UserID, Statuses: []RunStatus{RunMissed, RunFailed}, Limit: dashboardLimit(limit, DefaultProblems),
	})
	if err != nil {
		return nil, fmt.Errorf("recent run problems: %w", err)
	}
	return out, nil
}

// CountMissed counts the missed runs of actor's schedules scheduled in
// [from, until) (zero bounds are open).
func (s *Service) CountMissed(ctx context.Context, actor domain.Actor, from, until time.Time) (int64, error) {
	if actor.UserID.IsZero() {
		return 0, nil
	}
	n, err := s.repo.CountRuns(ctx, RunFilter{
		OwnerID: actor.UserID, Statuses: []RunStatus{RunMissed}, ScheduledFrom: from, ScheduledUntil: until,
	})
	if err != nil {
		return 0, fmt.Errorf("count missed runs: %w", err)
	}
	return n, nil
}

func dashboardLimit(limit, def int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, MaxDashboardList)
}

// ---------------------------------------------------------------------------
// Scheduler-facing use cases (no actor: the scheduler acts for the owner)
// ---------------------------------------------------------------------------

// OpenRuns returns at most limit (negative: every) pending and launching
// runs, oldest ScheduledFor first.
func (s *Service) OpenRuns(ctx context.Context, limit int) ([]Run, error) {
	rs, err := s.repo.ListOpenRuns(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list open runs: %w", err)
	}
	return rs, nil
}

// Load returns the schedules of ids that exist, by ID (deleted ones are
// missing).
func (s *Service) Load(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]Schedule, error) {
	out := make(map[domain.ScheduleID]Schedule, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ss, err := s.repo.GetSchedules(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("load schedules: %w", err)
	}
	for _, sc := range ss {
		out[sc.ID] = sc
	}
	return out, nil
}

// HasActiveRun reports whether the schedule has a launching run or a job
// that is queued, dispatched or running.
func (s *Service) HasActiveRun(ctx context.Context, id domain.ScheduleID) (bool, error) {
	active, err := s.repo.HasActiveRun(ctx, id)
	if err != nil {
		return false, fmt.Errorf("active run of schedule %s: %w", id, err)
	}
	return active, nil
}

// MarkLaunching reserves jobID on workerID for a pending run (pending ->
// launching), the first step of the two-phase launch. ErrConflict if the
// run is no longer pending (another tick or controller took it).
func (s *Service) MarkLaunching(ctx context.Context, run Run, jobID domain.JobID, workerID domain.WorkerID) (Run, error) {
	if jobID.IsZero() || workerID.IsZero() {
		return Run{}, fmt.Errorf("mark run %s launching: %w: job and worker are required", run.ID, domain.ErrInvalidID)
	}
	return s.updateRun(ctx, "mark run launching", RunUpdate{
		ID: run.ID, From: RunPending, To: RunLaunching, WorkerID: workerID, JobID: jobID, At: s.clock.Now(),
	})
}

// MarkLaunched records that a launching run's job exists (launching ->
// launched). ErrConflict if the run is no longer launching.
func (s *Service) MarkLaunched(ctx context.Context, run Run) (Run, error) {
	return s.updateRun(ctx, "mark run launched", RunUpdate{ID: run.ID, From: RunLaunching, To: RunLaunched, At: s.clock.Now()})
}

// EndRun moves an open run (in run.Status) to status: missed or failed
// with reason and msg, or pending again (a launch to retry; reason and msg
// are then cleared). With pause the schedule is disabled in the same step
// (PausedReason reason). ErrConflict if the run is no longer in
// run.Status.
func (s *Service) EndRun(ctx context.Context, run Run, status RunStatus, reason, msg string, pause bool) (Run, error) {
	switch {
	case !run.Status.Open():
		return Run{}, fmt.Errorf("end run %s: %w: it is %s", run.ID, domain.ErrFailedPrecondition, run.Status)
	case status != RunMissed && status != RunFailed && status != RunPending:
		return Run{}, fmt.Errorf("end run %s: %w: status %q", run.ID, domain.ErrInvalidArgument, status)
	case status == RunPending:
		reason, msg, pause = "", "", false
	}
	return s.updateRun(ctx, "end run", RunUpdate{
		ID: run.ID, From: run.Status, To: status, Reason: reason, Message: msg, At: s.clock.Now(), PauseSchedule: pause,
	})
}

func (s *Service) updateRun(ctx context.Context, op string, u RunUpdate) (Run, error) {
	r, err := s.repo.UpdateRun(ctx, u)
	if err != nil {
		return Run{}, fmt.Errorf("%s %s: %w", op, u.ID, err)
	}
	if u.To != RunLaunching {
		s.log.InfoContext(ctx, "schedule run updated", "run_id", u.ID.String(), "schedule_id", r.ScheduleID.String(),
			"status", string(u.To), "reason", u.Reason, "paused", u.PauseSchedule, "job_id", r.JobID.String())
	}
	return r, nil
}
