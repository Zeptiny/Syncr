package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// SampleInterval is the minimum time between two stored progress
	// samples of a job (worker clock). Live updates are not limited.
	SampleInterval time.Duration
	// MaxSpecBytes bounds an encoded job spec.
	MaxSpecBytes int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{SampleInterval: 5 * time.Second, MaxSpecBytes: 1 << 20}
}

// Deps are the service's collaborators. All are required except Logger.
type Deps struct {
	Jobs     Repository
	Samples  SampleRepository
	Cipher   Cipher
	Clock    Clock
	IDs      IDGenerator
	Broker   Broker
	Notifier Notifier
	Waker    Waker
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Service implements the job use cases. It is safe for concurrent use; its
// only state is the in-memory sample rate limiter.
type Service struct {
	repo     Repository
	samples  SampleRepository
	cipher   Cipher
	clock    Clock
	ids      IDGenerator
	broker   Broker
	notifier Notifier
	waker    Waker
	log      *slog.Logger
	cfg      Config

	mu sync.Mutex
	// lastStored is the Time of the last stored sample per running job.
	lastStored map[domain.JobID]time.Time
}

// maxAttempts bounds the optimistic update retries of one change.
const maxAttempts = 8

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Jobs == nil || d.Samples == nil || d.Cipher == nil || d.Clock == nil || d.IDs == nil ||
		d.Broker == nil || d.Notifier == nil || d.Waker == nil {
		return nil, errors.New("jobs: missing dependency")
	}
	def := DefaultConfig()
	cfg.SampleInterval = cmp.Or(cfg.SampleInterval, def.SampleInterval)
	cfg.MaxSpecBytes = cmp.Or(cfg.MaxSpecBytes, def.MaxSpecBytes)
	if cfg.SampleInterval < 0 || cfg.MaxSpecBytes < 0 {
		return nil, fmt.Errorf("jobs: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Jobs, samples: d.Samples, cipher: d.Cipher, clock: d.Clock, ids: d.IDs,
		broker: d.Broker, notifier: d.Notifier, waker: d.Waker, log: log, cfg: cfg,
		lastStored: make(map[domain.JobID]time.Time),
	}, nil
}

// ---------------------------------------------------------------------------
// API use cases
// ---------------------------------------------------------------------------

// Create queues a job on a worker. The spec is encrypted before it is
// stored. ErrNotFound if the worker does not exist or is removed.
func (s *Service) Create(ctx context.Context, p CreateParams) (Job, error) {
	switch {
	case p.WorkerID.IsZero():
		return Job{}, fmt.Errorf("create job: %w: worker id", domain.ErrInvalidID)
	case !validKind(p.Kind):
		return Job{}, fmt.Errorf("create job: %w: kind %q", domain.ErrInvalidArgument, p.Kind)
	case len(p.Spec) == 0:
		return Job{}, fmt.Errorf("create job: %w: empty spec", domain.ErrInvalidArgument)
	case len(p.Spec) > s.cfg.MaxSpecBytes:
		return Job{}, fmt.Errorf("create job: %w: spec is %d bytes, at most %d allowed", domain.ErrInvalidArgument, len(p.Spec), s.cfg.MaxSpecBytes)
	}
	id := s.ids.NewJobID()
	enc, err := s.cipher.Encrypt(p.Spec, SpecAD(id))
	if err != nil {
		return Job{}, fmt.Errorf("create job: encrypt spec: %w", err)
	}
	now := s.clock.Now()
	j := Job{
		ID: id, WorkerID: p.WorkerID, Kind: p.Kind, Status: domain.JobQueued, SpecEnc: enc,
		CreatedAt: now, QueuedAt: now, ExitCode: -1, Version: 1,
	}
	if err := s.repo.CreateJob(ctx, j); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Job{}, fmt.Errorf("create job: worker %s: %w", p.WorkerID, err)
		}
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	s.log.InfoContext(ctx, "job queued", "job_id", id.String(), "worker_id", p.WorkerID.String(), "kind", p.Kind)
	s.published(ctx, Job{}, j)
	return j, nil
}

// Get returns a job.
func (s *Service) Get(ctx context.Context, id domain.JobID) (Job, error) {
	if id.IsZero() {
		return Job{}, fmt.Errorf("get job: %w", domain.ErrInvalidID)
	}
	j, err := s.repo.GetJob(ctx, id)
	if err != nil {
		return Job{}, fmt.Errorf("get job %s: %w", id, err)
	}
	return j, nil
}

// LatestSample returns the latest stored progress sample of a job.
func (s *Service) LatestSample(ctx context.Context, id domain.JobID) (Sample, bool, error) {
	smp, ok, err := s.samples.LatestJobSample(ctx, id)
	if err != nil {
		return Sample{}, false, fmt.Errorf("latest sample of job %s: %w", id, err)
	}
	return smp, ok, nil
}

// Samples returns the stored progress samples of a job, oldest first.
func (s *Service) Samples(ctx context.Context, id domain.JobID) ([]Sample, error) {
	out, err := s.samples.ListJobSamples(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("samples of job %s: %w", id, err)
	}
	return out, nil
}

// List returns a page of jobs matching f, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) (Page, error) {
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

// Cancel cancels a job: a queued job becomes cancelled at once; for a
// dispatched or running job a cancel is requested, the worker is woken to
// receive its CancelJob command, and the job becomes cancelled when the
// worker confirms. Canceling a finished job, or one already asked to stop,
// changes nothing. It returns the job after the call.
func (s *Service) Cancel(ctx context.Context, id domain.JobID) (Job, error) {
	if id.IsZero() {
		return Job{}, fmt.Errorf("cancel job: %w", domain.ErrInvalidID)
	}
	now := s.clock.Now()
	j, _, err := s.change(ctx, id, domain.WorkerID{}, func(j *Job) (bool, error) {
		switch {
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
	return j, nil
}

// Watch calls fn with the job's current state, then with every update until
// the job reaches a terminal status (at once if it already has), ctx ends,
// or fn returns an error, which Watch returns. Job updates are delivered in
// version order without duplicates; Progress updates only while the job
// runs.
func (s *Service) Watch(ctx context.Context, id domain.JobID, fn func(Update) error) error {
	if id.IsZero() {
		return fmt.Errorf("watch job: %w", domain.ErrInvalidID)
	}
	// Subscribe before reading, so no change between the read and the
	// subscription is lost; duplicates are filtered by version.
	sub := s.broker.Subscribe(id)
	defer sub.Close()
	j, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(Update{JobID: id, Job: strip(j)}); err != nil {
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
	s.broker.Publish(Update{JobID: cur.ID, Job: strip(cur)})
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
	if cur.Status == domain.JobFailed || cur.Status == domain.JobInterrupted {
		if err := s.notifier.JobFailed(ctx, *strip(cur)); err != nil {
			s.log.WarnContext(ctx, "job failure notification failed", "job_id", cur.ID.String(), "error", err.Error())
		}
	}
}

// strip returns a copy of j without its encrypted spec, for publishing.
func strip(j Job) *Job {
	j.SpecEnc = nil
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
