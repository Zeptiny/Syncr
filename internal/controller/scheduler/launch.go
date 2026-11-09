package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// maxMessage bounds the details stored with a failed run.
const maxMessage = 500

// pass is the state of one launch pass: its time and what it has looked
// up, so many runs cost few queries.
type pass struct {
	now    time.Time
	owners map[domain.UserID]bool
	// busy counts the unfinished jobs per worker, including the ones this
	// pass launched, so runs launched together spread over a label's
	// workers.
	busy   map[domain.WorkerID]int
	labels map[string][]workers.Worker
}

// launchOpen decides every open run, oldest first.
func (s *Scheduler) launchOpen(ctx context.Context) error {
	// A schedule has at most one pending run (and launching ones only
	// briefly), so this is bounded by the number of schedules; listing
	// them all lets no waiting run starve the ones behind it.
	runs, err := s.schedules.OpenRuns(ctx, -1)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	ids := make([]domain.ScheduleID, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ScheduleID)
	}
	scheds, err := s.schedules.Load(ctx, ids)
	if err != nil {
		return err
	}
	p := &pass{
		now: s.clock.Now(), owners: map[domain.UserID]bool{},
		busy: map[domain.WorkerID]int{}, labels: map[string][]workers.Worker{},
	}
	var errs []error
	for _, r := range runs {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		sc, ok := scheds[r.ScheduleID]
		if !ok {
			continue // deleted meanwhile, with its runs
		}
		if err := s.decide(ctx, p, sc, r); err != nil {
			errs = append(errs, fmt.Errorf("run %s of schedule %s: %w", r.ID, sc.ID, err))
		}
	}
	return errors.Join(errs...)
}

// decide handles one open run (see the package documentation for the
// order of the checks).
func (s *Scheduler) decide(ctx context.Context, p *pass, sc schedules.Schedule, r schedules.Run) error {
	if r.Status == schedules.RunLaunching {
		// The launch was decided (in time) and its job ID reserved:
		// complete it.
		return s.create(ctx, p, sc, r)
	}
	active := false
	if schedules.WaitsForActive(sc.Overlap) {
		var err error
		if active, err = s.schedules.HasActiveRun(ctx, sc.ID); err != nil {
			return err
		}
	}
	if p.now.After(r.Deadline) {
		reason := schedules.MissedReason(sc.Overlap, active)
		return s.end(ctx, r, schedules.RunMissed, reason, missedMessage(sc, reason), false)
	}
	if active {
		return nil // waits for the active run
	}
	ok, err := s.ownerActive(ctx, p, sc.OwnerID)
	if err != nil {
		return err
	}
	if !ok {
		return s.end(ctx, r, schedules.RunFailed, schedules.ReasonOwnerDisabled, "the schedule's owner is disabled", true)
	}
	if _, _, err := s.check(sc); err != nil {
		return s.end(ctx, r, schedules.RunFailed, schedules.ReasonInvalidDefinition, clip(err.Error()), true)
	}
	workerID, launch, err := s.target(ctx, p, sc, r)
	if err != nil || !launch {
		return err
	}
	jobID := r.JobID // kept from a launch that went back to pending
	if jobID.IsZero() {
		jobID = s.ids.NewJobID()
	}
	r, err = s.schedules.MarkLaunching(ctx, r, jobID, workerID)
	if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
		return nil // another pass or controller took it, or it was deleted
	}
	if err != nil {
		return err
	}
	return s.create(ctx, p, sc, r)
}

// target picks the run's worker: launch is false while the run waits, or
// when it was ended here (no such worker).
func (s *Scheduler) target(ctx context.Context, p *pass, sc schedules.Schedule, r schedules.Run) (domain.WorkerID, bool, error) {
	if id := sc.Target.WorkerID; !id.IsZero() {
		w, err := s.workers.Get(ctx, id)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return domain.WorkerID{}, false, s.end(ctx, r, schedules.RunFailed, schedules.ReasonWorkerRemoved,
				"the schedule's worker was removed; choose another worker and resume the schedule", true)
		case err != nil:
			return domain.WorkerID{}, false, err
		}
		return w.ID, s.workers.Status(w) == domain.WorkerOnline, nil
	}
	label := sc.Target.Label
	ws, ok := p.labels[label]
	if !ok {
		var err error
		if ws, err = s.workers.List(ctx, workers.ListFilter{Label: label}); err != nil {
			return domain.WorkerID{}, false, err
		}
		p.labels[label] = ws
	}
	var cands []candidate
	known := 0
	for _, w := range ws {
		if w.Removed() {
			continue
		}
		reported := len(w.Info.JobKinds) > 0
		runsKind := slices.Contains(w.Info.JobKinds, sc.Kind)
		if !reported || runsKind {
			known++ // may run the kind (a worker that never reported might)
		}
		if runsKind && s.workers.Status(w) == domain.WorkerOnline {
			busy, err := s.busy(ctx, p, w.ID)
			if err != nil {
				return domain.WorkerID{}, false, err
			}
			cands = append(cands, candidate{ID: w.ID, Name: w.Name, Busy: busy})
		}
	}
	if known == 0 {
		msg := fmt.Sprintf("no worker has the label %q", label)
		if len(ws) > 0 {
			msg = fmt.Sprintf("no worker with the label %q runs %s jobs", label, sc.Kind)
		}
		return domain.WorkerID{}, false, s.end(ctx, r, schedules.RunFailed, schedules.ReasonNoWorkerWithLabel, msg, false)
	}
	c, ok := pick(cands)
	return c.ID, ok, nil
}

// create creates a launching run's job with the reserved ID and closes the
// run.
func (s *Scheduler) create(ctx context.Context, p *pass, sc schedules.Schedule, r schedules.Run) error {
	kind, refs, err := s.check(sc)
	if err != nil {
		return s.end(ctx, r, schedules.RunFailed, schedules.ReasonInvalidDefinition, clip(err.Error()), true)
	}
	_, err = s.jobs.Create(ctx, domain.Actor{UserID: sc.OwnerID, Role: domain.RoleUser}, jobs.CreateParams{
		ID: r.JobID, ScheduleID: sc.ID, WorkerID: r.WorkerID, Kind: kind, Definition: sc.Definition,
		RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
	})
	switch {
	case err == nil:
		if n, ok := p.busy[r.WorkerID]; ok {
			p.busy[r.WorkerID] = n + 1 // a later lookup reads the stored job
		}
	case errors.Is(err, domain.ErrAlreadyExists):
		// An earlier attempt created it.
	case errors.Is(err, jobs.ErrUnknownRemote):
		return s.end(ctx, r, schedules.RunFailed, schedules.ReasonRemoteUnavailable,
			"a remote of the job was deleted or is not the owner's; edit the schedule and resume it", true)
	case errors.Is(err, jobs.ErrUnknownWorker):
		// The worker went away since the decision: decide again, unless a
		// later occurrence opened a pending run meanwhile, which takes over.
		_, err := s.schedules.EndRun(ctx, r, schedules.RunPending, "", "", false)
		if errors.Is(err, domain.ErrAlreadyExists) {
			return s.end(ctx, r, schedules.RunFailed, schedules.ReasonWorkerRemoved,
				"the run's worker was removed while it launched; a later run of the schedule is pending", false)
		}
		if err == nil || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidID):
		return s.end(ctx, r, schedules.RunFailed, schedules.ReasonInvalidDefinition, clip(err.Error()), true)
	default:
		// Retried with the same job ID on the next pass.
		return fmt.Errorf("create job %s: %w", r.JobID, err)
	}
	if _, err := s.schedules.MarkLaunched(ctx, r); err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return nil
}

// check decodes and checks the schedule's stored definition with its job
// kind.
func (s *Scheduler) check(sc schedules.Schedule) (string, jobkind.Refs, error) {
	def, err := jobkind.Unmarshal(sc.Definition)
	if err != nil {
		return "", jobkind.Refs{}, err
	}
	return s.kinds.Check(def)
}

// end moves an open run to status; a lost race (another pass or
// controller moved it, or the schedule was deleted) is not an error.
func (s *Scheduler) end(ctx context.Context, r schedules.Run, status schedules.RunStatus, reason, msg string, pause bool) error {
	_, err := s.schedules.EndRun(ctx, r, status, reason, msg, pause)
	if err == nil || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
		if err == nil && pause {
			s.log.WarnContext(ctx, "schedule paused", "schedule_id", r.ScheduleID.String(), "run_id", r.ID.String(), "reason", reason)
		}
		return nil
	}
	return err
}

// ownerActive reports whether a schedule's owner may run jobs (cached per
// pass). A missing owner is not active.
func (s *Scheduler) ownerActive(ctx context.Context, p *pass, id domain.UserID) (bool, error) {
	if ok, seen := p.owners[id]; seen {
		return ok, nil
	}
	ok, err := s.owners.Active(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		ok, err = false, nil
	}
	if err != nil {
		return false, err
	}
	p.owners[id] = ok
	return ok, nil
}

// busy returns the number of a worker's unfinished jobs (cached per pass).
func (s *Scheduler) busy(ctx context.Context, p *pass, id domain.WorkerID) (int, error) {
	if n, ok := p.busy[id]; ok {
		return n, nil
	}
	js, err := s.jobs.Unfinished(ctx, id)
	if err != nil {
		return 0, err
	}
	p.busy[id] = len(js)
	return len(js), nil
}

// candidate is an online worker with the label that runs the kind.
type candidate struct {
	ID   domain.WorkerID
	Name string
	// Busy is its number of unfinished jobs.
	Busy int
}

// pick returns the least busy candidate, then the first by name
// (case-insensitive), then by ID; ok is false if there is none.
func pick(cands []candidate) (candidate, bool) {
	if len(cands) == 0 {
		return candidate{}, false
	}
	return slices.MinFunc(cands, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.Busy, b.Busy), cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.ID.String(), b.ID.String()))
	}), true
}

// missedMessage explains a missed run.
func missedMessage(sc schedules.Schedule, reason string) string {
	switch {
	case reason == schedules.ReasonOverlap:
		return "an earlier run of the schedule was still active at the deadline"
	case sc.Target.Label != "":
		return fmt.Sprintf("no online worker with the label %q took the run before its deadline", sc.Target.Label)
	default:
		return "the worker was offline until the run's deadline"
	}
}

// clip bounds a stored message.
func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
