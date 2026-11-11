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
	// SampleRetention is how long the progress samples of a finished job
	// are kept (jobs.sample_retention): PurgeSamples deletes the samples of
	// the jobs that finished longer ago. At least MinSampleRetention.
	SampleRetention time.Duration
	// KeepSamples turns the purge off: PurgeSamples deletes nothing and
	// SampleRetention is ignored (jobs.sample_retention: 0). It is a
	// separate field so that the zero Config keeps the default retention.
	KeepSamples bool
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{SampleInterval: 5 * time.Second, MaxDefinitionBytes: 1 << 20, SampleRetention: DefaultSampleRetention}
}

// Deps are the service's collaborators. All are required except Remotes,
// Repositories, Activity and Logger.
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
	// Repositories checks the restic repository of new jobs. Without it,
	// creating a job on a repository fails with ErrUnknownRepository (fail
	// closed).
	Repositories RepositoryChecker
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
	repos    RepositoryChecker
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
	cfg.SampleRetention = cmp.Or(cfg.SampleRetention, def.SampleRetention)
	if cfg.SampleInterval < 0 || cfg.MaxDefinitionBytes < 0 {
		return nil, fmt.Errorf("jobs: invalid config %+v", cfg)
	}
	if !cfg.KeepSamples && cfg.SampleRetention < MinSampleRetention {
		return nil, fmt.Errorf("jobs: invalid config: sample retention %v is shorter than %v", cfg.SampleRetention, MinSampleRetention)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Jobs, samples: d.Samples, clock: d.Clock, ids: d.IDs,
		broker: d.Broker, notifier: d.Notifier, waker: d.Waker, remotes: d.Remotes, repos: d.Repositories, activity: d.Activity,
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
	// ErrUnknownRepository: the definition's restic repository is not a
	// live repository of the actor.
	ErrUnknownRepository = fmt.Errorf("repository %w", domain.ErrNotFound)
)

// ErrRepositoryDeleting reports a job on a restic repository whose data is
// being deleted: only the deletion job itself may be created. It matches
// domain.ErrFailedPrecondition.
var ErrRepositoryDeleting = fmt.Errorf("the repository's data is being deleted: %w", domain.ErrFailedPrecondition)

// ---------------------------------------------------------------------------
// API use cases
// ---------------------------------------------------------------------------
//
// Jobs belong to the actor who created them. An actor may see and cancel
// their own jobs; an admin may see and cancel every job. Another user's job
// is ErrNotFound, so its existence does not leak.

// Create queues a job of the actor on a worker. The definition's remotes
// must be live remotes of the actor (ErrUnknownRemote otherwise) and its
// repository a live repository of the actor (ErrUnknownRepository
// otherwise; ErrRepositoryDeleting while its data is being deleted, unless
// p.ID is the deletion's reserved job ID); the repository's remote joins
// RemoteIDs and its name the search text. The worker must exist and not be
// removed (ErrUnknownWorker otherwise). The concurrency keys are validated
// (domain.ErrInvalidArgument), sorted and deduplicated. A preset p.ID is
// used as the job's ID (ErrAlreadyExists if it is taken); p.ScheduleID
// links the job to the schedule launching it.
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
	keys, err := normalizeKeys(p.ConcurrencyKeys)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	for _, id := range p.RemoteIDs {
		if id.IsZero() {
			return Job{}, fmt.Errorf("create job: %w: remote id", domain.ErrInvalidID)
		}
	}
	terms := p.SearchTerms
	remoteIDs := p.RemoteIDs
	if !p.RepositoryID.IsZero() {
		name, remoteID, err := s.usableRepository(ctx, actor.UserID, p.RepositoryID, p.ID)
		if err != nil {
			return Job{}, fmt.Errorf("create job: %w", err)
		}
		remoteIDs = append(slices.Clone(remoteIDs), remoteID)
		terms = append(slices.Clone(terms), name)
	}
	remoteIDs = sortedIDs(remoteIDs)
	var names map[domain.RemoteID]string
	if len(remoteIDs) > 0 {
		if s.remotes == nil {
			return Job{}, fmt.Errorf("create job: %w: remotes are not available", ErrUnknownRemote)
		}
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
		Definition: slices.Clone(p.Definition), RemoteIDs: remoteIDs, SearchText: searchText(p.Kind, terms, remoteIDs, names),
		RepositoryID: p.RepositoryID, ConcurrencyKeys: keys,
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

// usableRepository checks the repository of a new job (see Create).
func (s *Service) usableRepository(ctx context.Context, owner domain.UserID, id domain.ResticRepoID, jobID domain.JobID) (string, domain.RemoteID, error) {
	if s.repos == nil {
		return "", domain.RemoteID{}, fmt.Errorf("%w: repositories are not available", ErrUnknownRepository)
	}
	name, remoteID, err := s.repos.UsableRepository(ctx, owner, id, jobID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "", domain.RemoteID{}, fmt.Errorf("%w: %w", ErrUnknownRepository, err)
	case errors.Is(err, domain.ErrFailedPrecondition):
		return "", domain.RemoteID{}, fmt.Errorf("%w: %w", ErrRepositoryDeleting, err)
	case err != nil:
		return "", domain.RemoteID{}, fmt.Errorf("check the repository: %w", err)
	case remoteID.IsZero():
		return "", domain.RemoteID{}, fmt.Errorf("check the repository %s: %w: it has no remote", id, domain.ErrFailedPrecondition)
	}
	return name, remoteID, nil
}

// Blockers returns, for each job of ids that has concurrency keys, the
// older unfinished job it waits for (the oldest one sharing one of its
// keys); jobs that wait for nothing are omitted. It does not check access:
// call it only for jobs the actor has read (the API's
// Job.waiting_for_job_id) or from the dispatcher. At most MaxBlockerIDs
// IDs.
func (s *Service) Blockers(ctx context.Context, ids []domain.JobID) (map[domain.JobID]domain.JobID, error) {
	if len(ids) == 0 {
		return map[domain.JobID]domain.JobID{}, nil
	}
	if len(ids) > MaxBlockerIDs {
		return nil, fmt.Errorf("job blockers: %w: at most %d jobs", domain.ErrInvalidArgument, MaxBlockerIDs)
	}
	uniq := slices.Clone(ids)
	slices.SortFunc(uniq, func(a, b domain.JobID) int { return a.UUID().Compare(b.UUID()) })
	uniq = slices.Compact(uniq)
	for _, id := range uniq {
		if id.IsZero() {
			return nil, fmt.Errorf("job blockers: %w", domain.ErrInvalidID)
		}
	}
	out, err := s.repo.ListJobBlockers(ctx, uniq)
	if err != nil {
		return nil, fmt.Errorf("job blockers: %w", err)
	}
	return out, nil
}

// ValidConcurrencyKey reports whether key is a valid concurrency key:
// ^[a-z0-9][a-z0-9:._-]{0,199}$ (e.g. "restic-repo:<uuid>").
func ValidConcurrencyKey(key string) bool {
	if key == "" || len(key) > 200 {
		return false
	}
	for i := range len(key) {
		c := key[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case i > 0 && (c == ':' || c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// normalizeKeys validates keys and returns them sorted, without duplicates
// (nil for none).
func normalizeKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	out := slices.Clone(keys)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxConcurrencyKeys {
		return nil, fmt.Errorf("%w: %d concurrency keys, at most %d allowed", domain.ErrInvalidArgument, len(out), MaxConcurrencyKeys)
	}
	for _, k := range out {
		if !ValidConcurrencyKey(k) {
			return nil, fmt.Errorf("%w: concurrency key %q", domain.ErrInvalidArgument, k)
		}
	}
	return out, nil
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
// oldest first: ListSamples with DefaultSamplePoints, without the total.
func (s *Service) Samples(ctx context.Context, actor domain.Actor, id domain.JobID) ([]Sample, error) {
	out, _, err := s.ListSamples(ctx, actor, id, DefaultSamplePoints)
	return out, err
}

// ListSamples returns the stored progress samples of a job the actor may
// see, oldest first, evenly thinned to at most maxPoints (always with the
// first and the last; see SampleRepository.ListJobSamples), and how many
// the job has stored. maxPoints 0 means DefaultSamplePoints; more than
// MaxSamplePoints is capped; a negative one is ErrInvalidArgument.
func (s *Service) ListSamples(ctx context.Context, actor domain.Actor, id domain.JobID, maxPoints int) (samples []Sample, total int, err error) {
	switch {
	case maxPoints < 0:
		return nil, 0, fmt.Errorf("samples of job %s: %w: max points %d is negative", id, domain.ErrInvalidArgument, maxPoints)
	case maxPoints == 0:
		maxPoints = DefaultSamplePoints
	}
	maxPoints = min(maxPoints, MaxSamplePoints)
	if _, err := s.Get(ctx, actor, id); err != nil {
		return nil, 0, err
	}
	out, total, err := s.samples.ListJobSamples(ctx, id, maxPoints)
	if err != nil {
		return nil, 0, fmt.Errorf("samples of job %s: %w", id, err)
	}
	return out, total, nil
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
	s.wakeWaiting(ctx, cur)
	s.log.InfoContext(ctx, "job finished", "job_id", cur.ID.String(), "worker_id", cur.WorkerID.String(),
		"status", string(cur.Status), "error_code", cur.Error.Code)
	s.notifier.JobFinished(*copyOf(cur))
}

// wakeWaiting wakes the workers of the queued jobs that share a
// concurrency key with the finished job j: one of them may be the new head
// of its keys. A failure is only logged; workers ask for their commands
// again within seconds anyway.
func (s *Service) wakeWaiting(ctx context.Context, j Job) {
	if len(j.ConcurrencyKeys) == 0 {
		return
	}
	ids, err := s.repo.ListWorkersWaitingOnKeys(ctx, j.ConcurrencyKeys)
	if err != nil {
		s.log.WarnContext(ctx, "cannot wake the workers waiting for a finished job's concurrency keys",
			"job_id", j.ID.String(), "error", err.Error())
		return
	}
	for _, id := range ids {
		s.waker.Wake(id)
	}
}

// copyOf returns a copy of j for publishing. Jobs hold no secrets (the
// definition is secret-free), so nothing is removed.
func copyOf(j Job) *Job {
	j.Definition = slices.Clone(j.Definition)
	j.RemoteIDs = slices.Clone(j.RemoteIDs)
	j.ConcurrencyKeys = slices.Clone(j.ConcurrencyKeys)
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
