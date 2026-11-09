package schedules

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/cronexpr"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// FireBatch is how many due schedules FireDue reads per query.
	FireBatch int
	// MaxDefinitionBytes bounds an encoded job definition (as the jobs
	// service does).
	MaxDefinitionBytes int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{FireBatch: 100, MaxDefinitionBytes: 1 << 20}
}

// Deps are the service's collaborators. All are required except
// Repositories and Logger.
type Deps struct {
	Repo     Repository
	Remotes  RemoteChecker
	Contacts ContactChecker
	Workers  WorkerChecker
	// Repositories checks the restic repository of a definition. Without
	// it, a schedule on a repository fails with ErrUnknownRepository (fail
	// closed).
	Repositories RepositoryChecker
	// Waker is told when a schedule was saved or a run was added, so the
	// scheduler runs a pass soon instead of on its next tick.
	Waker Waker
	Clock Clock
	IDs   IDGenerator
	// Logger defaults to discarding. The service never logs definitions.
	Logger *slog.Logger
}

// Service implements the schedule use cases (see the package
// documentation). It is stateless apart from its dependencies and safe for
// concurrent use.
type Service struct {
	repo     Repository
	remotes  RemoteChecker
	contacts ContactChecker
	workers  WorkerChecker
	repos    RepositoryChecker
	waker    Waker
	clock    Clock
	ids      IDGenerator
	log      *slog.Logger
	cfg      Config
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Repo == nil || d.Remotes == nil || d.Contacts == nil || d.Workers == nil ||
		d.Waker == nil || d.Clock == nil || d.IDs == nil {
		return nil, errors.New("schedules: missing dependency")
	}
	def := DefaultConfig()
	cfg.FireBatch = cmp.Or(cfg.FireBatch, def.FireBatch)
	cfg.MaxDefinitionBytes = cmp.Or(cfg.MaxDefinitionBytes, def.MaxDefinitionBytes)
	if cfg.FireBatch < 1 || cfg.MaxDefinitionBytes < 1 {
		return nil, fmt.Errorf("schedules: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Repo, remotes: d.Remotes, contacts: d.Contacts, workers: d.Workers, repos: d.Repositories,
		waker: d.Waker, clock: d.Clock, ids: d.IDs, log: log, cfg: cfg,
	}, nil
}

// Errors of Create and Update that say which reference is missing. Both
// match domain.ErrNotFound, so another user's remote or contact looks like
// a missing one.
var (
	// ErrUnknownRemote: a remote of the definition is not a live remote of
	// the actor.
	ErrUnknownRemote = fmt.Errorf("remote %w", domain.ErrNotFound)
	// ErrUnknownContact: a linked contact is not one of the actor's.
	ErrUnknownContact = fmt.Errorf("contact %w", domain.ErrNotFound)
	// ErrUnknownRepository: the definition's restic repository is not a
	// live repository of the actor.
	ErrUnknownRepository = fmt.Errorf("repository %w", domain.ErrNotFound)
)

// ErrRepositoryDeleting reports a definition on a restic repository whose
// data is being deleted (Create, Update). It matches
// domain.ErrFailedPrecondition.
var ErrRepositoryDeleting = fmt.Errorf("the repository's data is being deleted: %w", domain.ErrFailedPrecondition)

// errForeign reports another user's schedule (as not found).
var errForeign = fmt.Errorf("schedule of another user: %w", domain.ErrNotFound)

// Create validates and stores a new schedule of actor. The schedule's next
// run is the expression's first occurrence after now if it is enabled.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in Input) (Schedule, error) {
	if actor.UserID.IsZero() {
		return Schedule{}, fmt.Errorf("create schedule: %w: schedules belong to a user account", domain.ErrPermissionDenied)
	}
	c, err := s.check(ctx, actor.UserID, in)
	if err != nil {
		return Schedule{}, fmt.Errorf("create schedule: %w", err)
	}
	now := s.clock.Now()
	sc := Schedule{ID: s.ids.NewScheduleID(), OwnerID: actor.UserID, CreatedAt: now, Version: 1}
	c.apply(&sc, now)
	if sc.Enabled {
		sc.NextRunAt = c.expr.Next(now)
	}
	if err := s.repo.CreateSchedule(ctx, sc); err != nil {
		return Schedule{}, fmt.Errorf("create schedule: %w", nameTaken(err, sc.Name))
	}
	s.log.InfoContext(ctx, "schedule created", "schedule_id", sc.ID.String(), "owner_id", actor.UserID.String(),
		"kind", sc.Kind, "enabled", sc.Enabled)
	s.waker.Wake()
	return sc, nil
}

// Get returns one of actor's schedules: ErrNotFound for another user's,
// admins included.
func (s *Service) Get(ctx context.Context, actor domain.Actor, id domain.ScheduleID) (Schedule, error) {
	sc, err := s.owned(ctx, actor, id)
	if err != nil {
		return Schedule{}, fmt.Errorf("get schedule: %w", err)
	}
	return sc, nil
}

// owned returns schedule id of actor, or ErrNotFound.
func (s *Service) owned(ctx context.Context, actor domain.Actor, id domain.ScheduleID) (Schedule, error) {
	if id.IsZero() {
		return Schedule{}, fmt.Errorf("schedule: %w", domain.ErrInvalidID)
	}
	sc, err := s.repo.GetSchedule(ctx, id)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule %s: %w", id, err)
	}
	if !actor.Owns(sc.OwnerID) {
		return Schedule{}, fmt.Errorf("schedule %s: %w", id, errForeign)
	}
	return sc, nil
}

// List returns actor's schedules matching f, by name; f.OwnerID is forced
// to the actor.
func (s *Service) List(ctx context.Context, actor domain.Actor, f ListFilter) ([]Schedule, error) {
	if actor.UserID.IsZero() {
		return []Schedule{}, nil
	}
	f.OwnerID = actor.UserID
	out, err := s.repo.ListSchedules(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return out, nil
}

// Update changes one of actor's schedules; version is the one the caller
// read (ErrConflict if it changed since). Changing Cron, Timezone or
// Enabled recomputes NextRunAt (from now; zero when disabled); otherwise
// the stored fire state is kept, so an edit cannot rewind or skip a fire.
// Enabling clears PausedReason; disabling cancels the open run.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id domain.ScheduleID, version int64, in Input) (Schedule, error) {
	cur, err := s.owned(ctx, actor, id)
	if err != nil {
		return Schedule{}, fmt.Errorf("update schedule: %w", err)
	}
	if version != cur.Version {
		return Schedule{}, fmt.Errorf("update schedule %s: at version %d, not %d: %w", id, cur.Version, version, domain.ErrConflict)
	}
	c, err := s.check(ctx, cur.OwnerID, in)
	if err != nil {
		return Schedule{}, fmt.Errorf("update schedule %s: %w", id, err)
	}
	now := s.clock.Now()
	next := cur
	c.apply(&next, now)
	reschedule := next.Cron != cur.Cron || next.Timezone != cur.Timezone || next.Enabled != cur.Enabled
	switch {
	case !next.Enabled:
		next.NextRunAt = time.Time{}
		if cur.Enabled {
			next.PausedReason = "" // paused by the user
		}
	case reschedule:
		next.NextRunAt = c.expr.Next(now)
		next.PausedReason = ""
	}
	stored, err := s.repo.UpdateSchedule(ctx, next, reschedule)
	if err != nil {
		return Schedule{}, fmt.Errorf("update schedule %s: %w", id, nameTaken(err, next.Name))
	}
	s.log.InfoContext(ctx, "schedule updated", "schedule_id", id.String(), "version", stored.Version,
		"enabled", stored.Enabled, "rescheduled", reschedule)
	s.waker.Wake()
	return stored, nil
}

// SetEnabled pauses or resumes one of actor's schedules without a version
// check. Resuming clears PausedReason and sets NextRunAt to the next
// occurrence after now; pausing clears NextRunAt and cancels the open run
// (without a notification). A schedule already in the requested state is
// returned unchanged, so a retried call cannot move NextRunAt past a due
// occurrence.
func (s *Service) SetEnabled(ctx context.Context, actor domain.Actor, id domain.ScheduleID, enabled bool) (Schedule, error) {
	cur, err := s.owned(ctx, actor, id)
	if err != nil {
		return Schedule{}, fmt.Errorf("set schedule enabled: %w", err)
	}
	if cur.Enabled == enabled {
		return cur, nil
	}
	now := s.clock.Now()
	t := Toggle{Enabled: enabled, At: now}
	if enabled {
		e, err := cronexpr.Parse(cur.Cron, cur.Timezone)
		if err != nil {
			// The stored expression no longer parses (e.g. the host lost its
			// zone data): resuming would never fire.
			return Schedule{}, fmt.Errorf("resume schedule %s: %w: %w", id, domain.ErrFailedPrecondition, err)
		}
		t.NextRunAt = e.Next(now)
	}
	stored, err := s.repo.SetScheduleEnabled(ctx, id, t)
	if err != nil {
		return Schedule{}, fmt.Errorf("set schedule %s enabled: %w", id, err)
	}
	s.log.InfoContext(ctx, "schedule enabled changed", "schedule_id", id.String(), "enabled", enabled)
	s.waker.Wake()
	return stored, nil
}

// Delete deletes one of actor's schedules with its runs; the jobs it
// launched are kept (their ScheduleID is cleared).
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.ScheduleID) error {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	if err := s.repo.DeleteSchedule(ctx, id); err != nil {
		return fmt.Errorf("delete schedule %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "schedule deleted", "schedule_id", id.String())
	return nil
}

// Preview checks an expression and returns its next count occurrences
// (1 to cronexpr.MaxPreview; 0 means DefaultPreview) after after (zero:
// now). Errors are *domain.ValidationError ("cron", "timezone", "count").
func (s *Service) Preview(cron, timezone string, after time.Time, count int) (Preview, error) {
	switch {
	case count == 0:
		count = DefaultPreview
	case count < 0 || count > cronexpr.MaxPreview:
		var v domain.Validation
		v.Add("count", fmt.Sprintf("must be between 1 and %d", cronexpr.MaxPreview))
		return Preview{}, v.Err()
	}
	e, err := cronexpr.Parse(cron, timezone)
	if err != nil {
		return Preview{}, err
	}
	if after.IsZero() {
		after = s.clock.Now()
	}
	return Preview{NextRuns: e.NextN(after, count), Normalized: e.String()}, nil
}

// DefaultPreview is the number of occurrences Preview returns by default.
const DefaultPreview = 5

// Names returns display references of the schedules of ids that exist, of
// any owner (the schedule of a job an admin sees). Missing ones are
// omitted.
func (s *Service) Names(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]Ref, error) {
	out := make(map[domain.ScheduleID]Ref, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ss, err := s.repo.GetSchedules(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("schedule names: %w", err)
	}
	for _, sc := range ss {
		out[sc.ID] = Ref{ID: sc.ID, Name: sc.Name}
	}
	return out, nil
}

// checked is a validated Input.
type checked struct {
	in   Input
	name string
	expr *cronexpr.Expr
	// remoteIDs and contacts are normalized: sorted by ID, without
	// duplicates.
	remoteIDs []domain.RemoteID
	contacts  []ContactLink
}

// apply writes the checked configuration into sc (UpdatedAt = now). The
// fire state, identity and Version are left to the caller.
func (c checked) apply(sc *Schedule, now time.Time) {
	sc.Name = c.name
	sc.Description = strings.TrimSpace(c.in.Description)
	sc.Cron, sc.Timezone = c.expr.String(), c.in.Timezone
	sc.Kind, sc.Definition = c.in.Kind, slices.Clone(c.in.Definition)
	sc.RemoteIDs, sc.SearchText = c.remoteIDs, strings.Join(c.in.SearchTerms, "\n")
	sc.RepositoryID = c.in.RepositoryID
	sc.Target = Target{WorkerID: c.in.Target.WorkerID, Label: strings.ToLower(c.in.Target.Label)}
	sc.Missed = c.in.Missed
	if sc.Missed.Action == MissedSkip {
		sc.Missed.MaxDelay = 0 // only run_late waits
	}
	sc.Overlap = c.in.Overlap.orDefault()
	sc.Enabled = c.in.Enabled
	sc.Contacts = c.contacts
	sc.UpdatedAt = now
}

// check validates in for owner: the fields (a *domain.ValidationError with
// the API's field paths), then the references, which must be owner's
// (ErrUnknownRemote, ErrUnknownRepository, ErrRepositoryDeleting,
// ErrUnknownContact).
func (s *Service) check(ctx context.Context, owner domain.UserID, in Input) (checked, error) {
	var v domain.Validation
	c := checked{in: in, name: checkName(&v, in.Name)}
	switch desc := strings.TrimSpace(in.Description); {
	case !utf8.ValidString(desc):
		v.Add("description", "is not valid UTF-8")
	case utf8.RuneCountInString(desc) > MaxDescriptionLen:
		v.Add("description", fmt.Sprintf("must be at most %d characters", MaxDescriptionLen))
	case strings.ContainsRune(desc, 0):
		v.Add("description", "must not contain NUL characters")
	}
	e, err := cronexpr.Parse(in.Cron, in.Timezone)
	var verr *domain.ValidationError
	switch {
	case errors.As(err, &verr):
		v.AddAll("", verr.Fields)
	case err != nil:
		return checked{}, err
	}
	c.expr = e
	s.checkTarget(ctx, &v, in.Target)
	switch {
	case !in.Missed.Action.Valid():
		v.Add("missed_run.action", "must be run_late or skip")
	case in.Missed.MaxDelay < 0 || in.Missed.MaxDelay > MaxMaxDelay:
		v.Add("missed_run.max_delay", fmt.Sprintf("must be between 0 (until the next run) and %v", MaxMaxDelay))
	}
	if in.Overlap != "" && !in.Overlap.Valid() {
		v.Add("overlap", "must be skip, queue or allow")
	}
	switch {
	case in.Kind == "" || len(in.Definition) == 0:
		v.Add("definition", "is required")
	case len(in.Definition) > s.cfg.MaxDefinitionBytes:
		v.Add("definition", fmt.Sprintf("is %d bytes, at most %d are allowed", len(in.Definition), s.cfg.MaxDefinitionBytes))
	}
	for _, id := range in.RemoteIDs {
		if id.IsZero() {
			v.Add("definition", "refers to an invalid remote ID")
			break
		}
	}
	c.remoteIDs = sortedIDs(in.RemoteIDs)
	c.contacts = checkContacts(&v, in.Contacts)
	if err := v.Err(); err != nil {
		return checked{}, err
	}
	if len(c.remoteIDs) > 0 {
		if _, err := s.remotes.Usable(ctx, owner, c.remoteIDs); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return checked{}, fmt.Errorf("%w: %w", ErrUnknownRemote, err)
			}
			return checked{}, fmt.Errorf("check remotes: %w", err)
		}
	}
	if !in.RepositoryID.IsZero() {
		if err := s.checkRepository(ctx, owner, in.RepositoryID); err != nil {
			return checked{}, err
		}
	}
	if len(c.contacts) > 0 {
		ids := make([]domain.ContactID, len(c.contacts))
		for i, l := range c.contacts {
			ids[i] = l.ContactID
		}
		if _, err := s.contacts.Usable(ctx, owner, ids); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return checked{}, fmt.Errorf("%w: %w", ErrUnknownContact, err)
			}
			return checked{}, fmt.Errorf("check contacts: %w", err)
		}
	}
	return c, nil
}

// checkRepository checks that id is a live repository of owner whose data
// is not being deleted.
func (s *Service) checkRepository(ctx context.Context, owner domain.UserID, id domain.ResticRepoID) error {
	if s.repos == nil {
		return fmt.Errorf("%w: repositories are not available", ErrUnknownRepository)
	}
	_, _, err := s.repos.UsableRepository(ctx, owner, id, domain.JobID{})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrUnknownRepository, err)
	case errors.Is(err, domain.ErrFailedPrecondition):
		return fmt.Errorf("%w: %w", ErrRepositoryDeleting, err)
	case err != nil:
		return fmt.Errorf("check the repository: %w", err)
	}
	return nil
}

// checkTarget validates the target: exactly one of a live worker and a
// valid label. Workers are not owned, so a missing one is a field error.
func (s *Service) checkTarget(ctx context.Context, v *domain.Validation, t Target) {
	label := strings.ToLower(t.Label)
	switch {
	case t.WorkerID.IsZero() && label == "":
		v.Add("target", "is required: a worker or a worker label")
	case !t.WorkerID.IsZero() && label != "":
		v.Add("target", "must be a worker or a worker label, not both")
	case label != "":
		if !domain.ValidLabel(label) {
			v.Add("target.worker_label", `must be 1 to 32 characters of a-z, 0-9, ".", "_" and "-", starting with a letter or digit`)
		}
	default:
		switch err := s.workers.CheckWorker(ctx, t.WorkerID); {
		case errors.Is(err, domain.ErrNotFound):
			v.Add("target.worker_id", "is not a worker (it may have been removed)")
		case err != nil:
			v.Add("target.worker_id", "could not be checked; try again")
			s.log.WarnContext(ctx, "cannot check a schedule's worker", "worker_id", t.WorkerID.String(), "error", err.Error())
		}
	}
}

// checkContacts validates and normalizes the contact links: sorted by
// contact ID, each once, with a non-empty set of schedule events.
func checkContacts(v *domain.Validation, links []ContactLink) []ContactLink {
	if len(links) > MaxContacts {
		v.Add("contacts", fmt.Sprintf("at most %d contacts are allowed", MaxContacts))
		return nil
	}
	out := make([]ContactLink, 0, len(links))
	for i, l := range links {
		switch {
		case l.ContactID.IsZero():
			v.Add(fmt.Sprintf("contacts[%d].contact_id", i), "is required")
		case slices.ContainsFunc(links[:i], func(o ContactLink) bool { return o.ContactID == l.ContactID }):
			v.Add(fmt.Sprintf("contacts[%d].contact_id", i), "is linked twice")
		}
		events := l.Events.Normalize()
		switch {
		case len(l.Events) == 0:
			v.Add(fmt.Sprintf("contacts[%d].events", i), "choose at least one event")
		case !l.Events.Valid() || !l.Events.Within(domain.ScheduleEvents()):
			v.Add(fmt.Sprintf("contacts[%d].events", i), "must be succeeded, failed, interrupted or missed, each once")
		}
		out = append(out, ContactLink{ContactID: l.ContactID, Events: events})
	}
	slices.SortFunc(out, func(a, b ContactLink) int { return strings.Compare(a.ContactID.String(), b.ContactID.String()) })
	if len(out) == 0 {
		return nil
	}
	return out
}

// checkName validates a schedule name and returns it without surrounding
// white space.
func checkName(v *domain.Validation, name string) string {
	name = strings.TrimSpace(name)
	switch n := utf8.RuneCountInString(name); {
	case !utf8.ValidString(name):
		v.Add("name", "is not valid UTF-8")
	case n == 0:
		v.Add("name", "is required")
	case n > MaxNameLen:
		v.Add("name", fmt.Sprintf("must be at most %d characters", MaxNameLen))
	case strings.ContainsFunc(name, unicode.IsControl):
		v.Add("name", "must not contain control characters")
	}
	return name
}

// nameTaken explains a repository ErrAlreadyExists (the only unique key a
// valid schedule can hit is the owner's names).
func nameTaken(err error, name string) error {
	if errors.Is(err, domain.ErrAlreadyExists) {
		return fmt.Errorf("you already have a schedule named %q: %w", name, err)
	}
	return err
}

// sortedIDs returns ids sorted, without duplicates (nil for none).
func sortedIDs[K any](ids []domain.ID[K]) []domain.ID[K] {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b domain.ID[K]) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
}
