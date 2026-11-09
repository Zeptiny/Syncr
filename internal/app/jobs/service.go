package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// SampleInterval is the minimum time between two stored progress
	// samples of a job (worker clock). Live updates are not limited.
	SampleInterval time.Duration
	// MaxDefinitionBytes bounds an encoded job definition.
	MaxDefinitionBytes int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{SampleInterval: 5 * time.Second, MaxDefinitionBytes: 1 << 20}
}

// Deps are the service's collaborators. All are required except Remotes
// and Logger.
type Deps struct {
	Jobs     Repository
	Samples  SampleRepository
	Clock    Clock
	IDs      IDGenerator
	Broker   Broker
	Notifier Notifier
	Waker    Waker
	// Remotes checks the remotes of new jobs. Without it, creating a job
	// that uses a remote fails with ErrUnknownRemote (fail closed); jobs
	// on the worker's filesystem only still work.
	Remotes RemoteChecker
	// Activity aggregates finished jobs for the dashboard (Activity,
	// CountUnfinished); optional, those use cases fail without it.
	Activity ActivityRepository
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Service implements the job use cases. It is safe for concurrent use; its
// only state is the in-memory sample rate limiter.
type Service struct {
	repo     Repository
	samples  SampleRepository
	clock    Clock
	ids      IDGenerator
	broker   Broker
	notifier Notifier
	waker    Waker
	remotes  RemoteChecker
	activity ActivityRepository
	log      *slog.Logger
	cfg      Config

	mu sync.Mutex
	// lastStored is the last stored sample per running job, for the rate
	// limiter; it is bounded (see staleSampleAfter). lastPrune is when
	// stale entries were last dropped.
	lastStored map[domain.JobID]lastSample
	lastPrune  time.Time
}

// maxAttempts bounds the optimistic update retries of one change.
const maxAttempts = 8

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Jobs == nil || d.Samples == nil || d.Clock == nil || d.IDs == nil ||
		d.Broker == nil || d.Notifier == nil || d.Waker == nil {
		return nil, errors.New("jobs: missing dependency")
	}
	def := DefaultConfig()
	cfg.SampleInterval = cmp.Or(cfg.SampleInterval, def.SampleInterval)
	cfg.MaxDefinitionBytes = cmp.Or(cfg.MaxDefinitionBytes, def.MaxDefinitionBytes)
	if cfg.SampleInterval < 0 || cfg.MaxDefinitionBytes < 0 {
		return nil, fmt.Errorf("jobs: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Jobs, samples: d.Samples, clock: d.Clock, ids: d.IDs,
		broker: d.Broker, notifier: d.Notifier, waker: d.Waker, remotes: d.Remotes, activity: d.Activity,
		log: log, cfg: cfg,
		lastStored: make(map[domain.JobID]lastSample), lastPrune: d.Clock.Now(),
	}, nil
}

// Errors of Create that say which reference is missing. Both match
// domain.ErrNotFound.
var (
	// ErrUnknownWorker: the worker does not exist or is removed.
	ErrUnknownWorker = fmt.Errorf("worker %w", domain.ErrNotFound)
	// ErrUnknownRemote: a remote of the definition is not a live remote of
	// the actor.
	ErrUnknownRemote = fmt.Errorf("remote %w", domain.ErrNotFound)
)

// ---------------------------------------------------------------------------
// API use cases
// ---------------------------------------------------------------------------
//
// Jobs belong to the actor who created them. An actor may see and cancel
// their own jobs; an admin may see and cancel every job. Another user's job
// is ErrNotFound, so its existence does not leak.

// Create queues a job of the actor on a worker. The definition's remotes
// must be live remotes of the actor (ErrUnknownRemote otherwise); the
// worker must exist and not be removed (ErrUnknownWorker otherwise). A
// preset p.ID is used as the job's ID (ErrAlreadyExists if it is taken);
// p.ScheduleID links the job to the schedule launching it.
func (s *Service) Create(ctx context.Context, actor domain.Actor, p CreateParams) (Job, error) {
	switch {
	case p.WorkerID.IsZero():
		return Job{}, fmt.Errorf("create job: %w: worker id", domain.ErrInvalidID)
	case actor.UserID.IsZero():
		return Job{}, fmt.Errorf("create job: %w: the actor is not a user", domain.ErrInvalidID)
	case !validKind(p.Kind):
		return Job{}, fmt.Errorf("create job: %w: kind %q", domain.ErrInvalidArgument, p.Kind)
	case len(p.Definition) == 0:
		return Job{}, fmt.Errorf("create job: %w: empty definition", domain.ErrInvalidArgument)
	case len(p.Definition) > s.cfg.MaxDefinitionBytes:
		return Job{}, fmt.Errorf("create job: %w: definition is %d bytes, at most %d allowed", domain.ErrInvalidArgument, len(p.Definition), s.cfg.MaxDefinitionBytes)
	}
	remoteIDs := sortedIDs(p.RemoteIDs)
	for _, id := range remoteIDs {
		if id.IsZero() {
			return Job{}, fmt.Errorf("create job: %w: remote id", domain.ErrInvalidID)
		}
	}
	var names map[domain.RemoteID]string
	if len(remoteIDs) > 0 {
		if s.remotes == nil {
			return Job{}, fmt.Errorf("create job: %w: remotes are not available", ErrUnknownRemote)
		}
		var err error
		if names, err = s.remotes.Usable(ctx, actor.UserID, remoteIDs); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return Job{}, fmt.Errorf("create job: %w: %w", ErrUnknownRemote, err)
			}
			return Job{}, fmt.Errorf("create job: check remotes: %w", err)
		}
	}
	id := p.ID
	if id.IsZero() {
		id = s.ids.NewJobID()
	}
	now := s.clock.Now()
	j := Job{
		ID: id, WorkerID: p.WorkerID, OwnerID: actor.UserID, ScheduleID: p.ScheduleID, Kind: p.Kind, Status: domain.JobQueued,
		Definition: slices.Clone(p.Definition), RemoteIDs: remoteIDs, SearchText: searchText(p.Kind, p.SearchTerms, remoteIDs, names),
		CreatedAt: now, QueuedAt: now, ExitCode: -1, Version: 1,
	}
	if err := s.repo.CreateJob(ctx, j); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Job{}, fmt.Errorf("create job: %w: %w", ErrUnknownWorker, err)
		}
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	s.log.InfoContext(ctx, "job queued", "job_id", id.String(), "worker_id", p.WorkerID.String(),
		"owner_id", actor.UserID.String(), "kind", p.Kind)
	s.published(ctx, Job{}, j)
	return j, nil
}

// Get returns a job the actor may see.
func (s *Service) Get(ctx context.Context, actor domain.Actor, id domain.JobID) (Job, error) {
	if id.IsZero() {
		return Job{}, fmt.Errorf("get job: %w", domain.ErrInvalidID)
	}
	j, err := s.repo.GetJob(ctx, id)
	if err != nil {
		return Job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	if !actor.CanView(j.OwnerID) {
		return Job{}, fmt.Errorf("get job %s: %w", id, errForeign)
	}
	return j, nil
}

// errForeign reports another user's job (as not found).
var errForeign = fmt.Errorf("job of another user: %w", domain.ErrNotFound)

// LatestSample returns the latest stored progress sample of a job. It does
// not check access: call it only for a job the actor has read (Get, List,
// Watch), e.g. to show its live counters.
func (s *Service) LatestSample(ctx context.Context, id domain.JobID) (Sample, bool, error) {
	smp, ok, err := s.samples.LatestJobSample(ctx, id)
	if err != nil {
		return Sample{}, false, fmt.Errorf("latest sample of job %s: %w", id, err)
	}
	return smp, ok, nil
}

// LatestSamples is LatestSample for several jobs in one store query, keyed
// by job ID (jobs without samples are absent). Like LatestSample it does
// not check access.
func (s *Service) LatestSamples(ctx context.Context, ids []domain.JobID) (map[domain.JobID]Sample, error) {
	if len(ids) == 0 {
		return map[domain.JobID]Sample{}, nil
	}
	smps, err := s.samples.LatestJobSamples(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("latest samples of %d jobs: %w", len(ids), err)
	}
	return smps, nil
}

// Samples returns the stored progress samples of a job the actor may see,
// oldest first.
func (s *Service) Samples(ctx context.Context, actor domain.Actor, id domain.JobID) ([]Sample, error) {
	if _, err := s.Get(ctx, actor, id); err != nil {
		return nil, err
	}
	out, err := s.samples.ListJobSamples(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("samples of job %s: %w", id, err)
	}
	return out, nil
}

// List returns a page of jobs matching f, newest first: the actor's own
// jobs, or with f.AllUsers every user's for an admin (f.OwnerID then
// narrows them to one user).
func (s *Service) List(ctx context.Context, actor domain.Actor, f ListFilter) (Page, error) {
	switch {
	case f.Limit == 0:
		f.Limit = DefaultPageSize
	case f.Limit < 0 || f.Limit > MaxPageSize:
		return Page{}, fmt.Errorf("list jobs: %w: limit %d outside [1, %d]", domain.ErrInvalidArgument, f.Limit, MaxPageSize)
	}
	for _, st := range f.Statuses {
		if !st.Valid() {
			return Page{}, fmt.Errorf("list jobs: %w: %q", domain.ErrInvalidJobStatus, st)
		}
	}
	if len(f.Kinds) > MaxKinds {
		return Page{}, fmt.Errorf("list jobs: %w: at most %d kinds", domain.ErrInvalidArgument, MaxKinds)
	}
	for _, k := range f.Kinds {
		if !validKind(k) {
			return Page{}, fmt.Errorf("list jobs: %w: kind %q", domain.ErrInvalidArgument, k)
		}
	}
	if len(f.Query) > MaxQueryLen || !utf8.ValidString(f.Query) || strings.ContainsRune(f.Query, 0) {
		return Page{}, fmt.Errorf("list jobs: %w: the query must be valid text of at most %d bytes", domain.ErrInvalidArgument, MaxQueryLen)
	}
	f.Query = strings.ToLower(strings.TrimSpace(f.Query))
	if !actor.IsAdmin() || !f.AllUsers {
		if actor.UserID.IsZero() {
			return Page{}, nil // nobody owns no jobs
		}
		f.OwnerID = actor.UserID
	}
	f.AllUsers = false
	limit := f.Limit
	f.Limit++ // one more tells whether a next page exists
	js, err := s.repo.ListJobs(ctx, f)
	if err != nil {
		return Page{}, fmt.Errorf("list jobs: %w", err)
	}
	var p Page
	if len(js) > limit {
		js = js[:limit]
		p.Next = js[limit-1].ID
	}
	p.Jobs = js
	return p, nil
}

// Cancel cancels a job the actor may see: a queued job becomes cancelled
// at once; for a dispatched or running job a cancel is requested, the
// worker is woken to receive its CancelJob command, and the job becomes
// cancelled when the worker confirms. Canceling a finished job, or one
// already asked to stop, changes nothing. It returns the job after the
// call.
func (s *Service) Cancel(ctx context.Context, actor domain.Actor, id domain.JobID) (Job, error) {
	if id.IsZero() {
		return Job{}, fmt.Errorf("cancel job: %w", domain.ErrInvalidID)
	}
	now := s.clock.Now()
	j, changed, err := s.change(ctx, id, domain.WorkerID{}, func(j *Job) (bool, error) {
		switch {
		case !actor.CanView(j.OwnerID):
			return false, errForeign
		case j.Status == domain.JobQueued:
			return true, finish(j, domain.JobCancelled, now, JobError{Code: CodeCancelled, Message: "cancelled before it was dispatched"})
		case j.Status.Active() && !j.CancelRequested():
			j.CancelRequestedAt = now
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return Job{}, fmt.Errorf("cancel job %s: %w", id, err)
	}
	if changed {
		s.log.InfoContext(ctx, "job cancel requested", "job_id", id.String(), "actor_id", actor.UserID.String(), "status", string(j.Status))
	}
	return j, nil
}

// Watch calls fn with the current state of a job the actor may see, then
// with every update until the job reaches a terminal status (at once if it
// already has), ctx ends, or fn returns an error, which Watch returns. Job
// updates are delivered in version order without duplicates; Progress
// updates only while the job runs.
func (s *Service) Watch(ctx context.Context, actor domain.Actor, id domain.JobID, fn func(Update) error) error {
	if id.IsZero() {
		return fmt.Errorf("watch job: %w", domain.ErrInvalidID)
	}
	// Subscribe before reading, so no change between the read and the
	// subscription is lost; duplicates are filtered by version.
	sub := s.broker.Subscribe(id)
	defer sub.Close()
	j, err := s.Get(ctx, actor, id)
	if err != nil {
		return err
	}
	if err := fn(Update{JobID: id, Job: copyOf(j)}); err != nil {
		return err
	}
	if j.Status.Terminal() {
		return nil
	}
	if j.Status == domain.JobRunning {
		if smp, ok, err := s.LatestSample(ctx, id); err != nil {
			return err
		} else if ok {
			if err := fn(Update{JobID: id, Progress: &smp}); err != nil {
				return err
			}
		}
	}
	version, status := j.Version, j.Status
	for {
		u, err := sub.Next(ctx)
		if err != nil {
			return fmt.Errorf("watch job %s: %w", id, err)
		}
		switch {
		case u.Job != nil:
			if u.Job.Version <= version {
				continue
			}
			version, status = u.Job.Version, u.Job.Status
		case u.Progress != nil:
			if status != domain.JobRunning {
				continue
			}
		default:
			continue
		}
		if err := fn(u); err != nil {
			return err
		}
		if status.Terminal() {
			return nil
		}
	}
}

// searchText is a job's Job.SearchText.
func searchText(kind string, terms []string, ids []domain.RemoteID, names map[domain.RemoteID]string) string {
	parts := make([]string, 0, 1+len(terms)+len(ids))
	parts = append(parts, kind)
	parts = append(parts, terms...)
	for _, id := range ids {
		if n := names[id]; n != "" {
			parts = append(parts, n)
		}
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

// sortedIDs returns ids sorted and without duplicates (nil if empty).
func sortedIDs(ids []domain.RemoteID) []domain.RemoteID {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b domain.RemoteID) int { return a.UUID().Compare(b.UUID()) })
	return slices.Compact(out)
}

// ---------------------------------------------------------------------------
// Change machinery
// ---------------------------------------------------------------------------

// change applies fn to the current job and stores the result with an
// optimistic update, retrying when another change won the race. fn reports
// whether it changed the job; if not, nothing is stored and the current job
// is returned with changed == false. If workerID is not zero the job must
// belong to that worker (ErrNotFound otherwise).
func (s *Service) change(ctx context.Context, id domain.JobID, workerID domain.WorkerID, fn func(*Job) (bool, error)) (Job, bool, error) {
	for range maxAttempts {
		cur, err := s.repo.GetJob(ctx, id)
		if err != nil {
			return Job{}, false, err
		}
		if !workerID.IsZero() && cur.WorkerID != workerID {
			return Job{}, false, fmt.Errorf("job of another worker: %w", domain.ErrNotFound)
		}
		next := cur
		changed, err := fn(&next)
		if err != nil || !changed {
			return cur, false, err
		}
		stored, err := s.repo.UpdateJob(ctx, next)
		if errors.Is(err, domain.ErrConflict) {
			continue
		}
		if err != nil {
			return Job{}, false, err
		}
		s.published(ctx, cur, stored)
		return stored, true, nil
	}
	return Job{}, false, fmt.Errorf("job %s changed concurrently %d times: %w", id, maxAttempts, domain.ErrConflict)
}

// published runs the side effects of a stored change from prev (zero for a
// new job) to cur.
func (s *Service) published(ctx context.Context, prev, cur Job) {
	s.broker.Publish(Update{JobID: cur.ID, Job: copyOf(cur)})
	if cur.Status == domain.JobQueued && prev.Status != domain.JobQueued ||
		cur.CancelRequested() && !prev.CancelRequested() && !cur.Status.Terminal() {
		s.waker.Wake(cur.WorkerID)
	}
	if !cur.Status.Terminal() || prev.Status == cur.Status {
		return
	}
	s.forget(cur.ID)
	s.log.InfoContext(ctx, "job finished", "job_id", cur.ID.String(), "worker_id", cur.WorkerID.String(),
		"status", string(cur.Status), "error_code", cur.Error.Code)
	s.notifier.JobFinished(*copyOf(cur))
}

// copyOf returns a copy of j for publishing. Jobs hold no secrets (the
// definition is secret-free), so nothing is removed.
func copyOf(j Job) *Job {
	j.Definition = slices.Clone(j.Definition)
	j.RemoteIDs = slices.Clone(j.RemoteIDs)
	j.Warnings = slices.Clone(j.Warnings)
	j.Output = slices.Clone(j.Output)
	return &j
}

// setStatus moves j to status to, enforcing the domain state machine.
func setStatus(j *Job, to domain.JobStatus) error {
	next, err := j.Status.TransitionTo(to)
	if err != nil {
		return fmt.Errorf("job %s: %w", j.ID, err)
	}
	j.Status = next
	return nil
}

// finish moves j to a terminal status decided by the controller.
func finish(j *Job, to domain.JobStatus, at time.Time, e JobError) error {
	if err := setStatus(j, to); err != nil {
		return err
	}
	j.FinishedAt = at
	j.Error = e
	j.ExitCode = -1
	return nil
}

func validKind(kind string) bool {
	if kind == "" || len(kind) > 64 {
		return false
	}
	return !slices.ContainsFunc([]byte(kind), func(c byte) bool {
		return (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_'
	})
}
