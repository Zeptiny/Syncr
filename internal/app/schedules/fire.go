package schedules

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/cronexpr"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// maxFirePasses bounds the queries of one FireDue call, so a schedule that
// keeps losing its compare-and-swap cannot hold a tick.
const maxFirePasses = 10

// FireDue fires every enabled schedule whose NextRunAt is due (<= now): for
// each one it plans the fire (PlanFire) and stores it with
// Repository.FireSchedule, a compare-and-swap of NextRunAt plus the run in
// one transaction, so concurrent ticks (or controllers) fire an occurrence
// once. A lost compare-and-swap is skipped: the winner fired it, or the
// user edited the schedule meanwhile. It returns how many schedules fired;
// errors of single schedules are joined and do not stop the others.
func (s *Service) FireDue(ctx context.Context) (int, error) {
	fired := 0
	var errs []error
	for range maxFirePasses {
		now := s.clock.Now()
		due, err := s.repo.ListDueSchedules(ctx, now, s.cfg.FireBatch)
		if err != nil {
			return fired, errors.Join(append(errs, fmt.Errorf("list due schedules: %w", err))...)
		}
		n := 0
		for _, sc := range due {
			ok, err := s.fire(ctx, sc, now)
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				n++
			}
		}
		fired += n
		if len(due) < s.cfg.FireBatch || n == 0 {
			break
		}
	}
	return fired, errors.Join(errs...)
}

// fire fires one due schedule; ok reports whether this call stored the
// fire.
func (s *Service) fire(ctx context.Context, sc Schedule, now time.Time) (bool, error) {
	e, err := cronexpr.Parse(sc.Cron, sc.Timezone)
	if err != nil {
		// The stored expression no longer parses (the host's zone data
		// lacks its time zone): it can never compute a next run. Pause it,
		// so the problem shows in the UI instead of a schedule that is
		// silently stuck due.
		_, perr := s.repo.SetScheduleEnabled(ctx, sc.ID, Toggle{Enabled: false, PausedReason: ReasonInvalidDefinition, At: now})
		s.log.ErrorContext(ctx, "schedule paused: its cron expression or time zone no longer parses",
			"schedule_id", sc.ID.String(), "error", err.Error())
		if perr != nil && !errors.Is(perr, domain.ErrNotFound) {
			return false, fmt.Errorf("pause schedule %s: %w", sc.ID, perr)
		}
		return false, nil
	}
	active := false
	if sc.Overlap.orDefault() == OverlapSkip {
		if active, err = s.repo.HasActiveRun(ctx, sc.ID); err != nil {
			return false, fmt.Errorf("fire schedule %s: %w", sc.ID, err)
		}
	}
	plan := PlanFire(e, sc.NextRunAt, now, sc.Missed, sc.Overlap, active)
	run := Run{
		ID: s.ids.NewScheduleRunID(), ScheduleID: sc.ID, Trigger: TriggerCron,
		ScheduledFor: plan.ScheduledFor, Deadline: plan.Deadline, Coalesced: plan.Coalesced,
		Status: RunPending, CreatedAt: now, UpdatedAt: now,
	}
	if plan.Skip {
		run.Status, run.Reason, run.Message = RunMissed, ReasonOverlap, "the previous run was still active"
	}
	stored, err := s.repo.FireSchedule(ctx, Fire{
		ID: sc.ID, ExpectedNext: sc.NextRunAt, NewNext: plan.NewNext, Run: run, Merge: !plan.Skip,
	})
	switch {
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrNotFound):
		// Another tick or controller fired it, or the user changed, paused
		// or deleted it: the next tick reads it again.
		return false, nil
	case errors.Is(err, domain.ErrAlreadyExists):
		// A manual run was opened between the merge and the insert: the
		// next tick merges into it.
		s.log.DebugContext(ctx, "schedule fire raced an open run; retrying on the next tick", "schedule_id", sc.ID.String())
		return false, nil
	case err != nil:
		return false, fmt.Errorf("fire schedule %s: %w", sc.ID, err)
	}
	s.log.InfoContext(ctx, "schedule fired", "schedule_id", sc.ID.String(), "run_id", stored.ID.String(),
		"scheduled_for", plan.ScheduledFor, "coalesced", plan.Coalesced, "status", string(stored.Status),
		"merged", stored.ID != run.ID, "next_run_at", plan.NewNext)
	return true, nil
}
