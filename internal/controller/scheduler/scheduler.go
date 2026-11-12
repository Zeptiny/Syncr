package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// Config tunes the scheduler. Zero fields take the DefaultConfig values.
type Config struct {
	// Tick is the time between two passes when nothing wakes the
	// scheduler. It bounds how late an occurrence fires and how long a
	// run waits after its worker came back.
	Tick time.Duration
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{Tick: 10 * time.Second}
}

// Deps are the scheduler's collaborators. All are required except Signal
// and Logger.
type Deps struct {
	Schedules Schedules
	Jobs      Jobs
	Workers   Workers
	// Kinds checks a schedule's stored definition again at launch (its
	// references and search terms).
	Kinds  *jobkind.Registry
	Owners Owners
	IDs    IDGenerator
	Clock  Clock
	// Signal wakes the scheduler. Pass the one given to the services that
	// wake it (schedules.Deps.Waker, the jobs Notifier fan-out); New makes
	// one if it is nil.
	Signal *Signal
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Scheduler fires due schedules and launches their runs (see the package
// documentation). It is safe for concurrent use; its passes run one at a
// time.
type Scheduler struct {
	schedules Schedules
	jobs      Jobs
	workers   Workers
	kinds     *jobkind.Registry
	owners    Owners
	ids       IDGenerator
	clock     Clock
	signal    *Signal
	log       *slog.Logger
	cfg       Config

	// mu serializes passes (Run's and direct Tick calls).
	mu sync.Mutex
}

// New returns a Scheduler.
func New(d Deps, cfg Config) (*Scheduler, error) {
	if d.Schedules == nil || d.Jobs == nil || d.Workers == nil || d.Kinds == nil || d.Owners == nil ||
		d.IDs == nil || d.Clock == nil {
		return nil, errors.New("scheduler: missing dependency")
	}
	cfg.Tick = cmp.Or(cfg.Tick, DefaultConfig().Tick)
	if cfg.Tick < 0 {
		return nil, fmt.Errorf("scheduler: invalid config %+v", cfg)
	}
	sig := d.Signal
	if sig == nil {
		sig = NewSignal()
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{
		schedules: d.Schedules, jobs: d.Jobs, workers: d.Workers, kinds: d.Kinds, owners: d.Owners,
		ids: d.IDs, clock: d.Clock, signal: sig, log: log, cfg: cfg,
	}, nil
}

// Signal wakes a Scheduler. It exists before the scheduler does, which
// breaks the construction cycle between the scheduler (which needs the
// schedules service) and the schedules service (which wakes the
// scheduler): both get the same Signal. The zero value is not usable; use
// NewSignal.
type Signal struct {
	ch chan struct{}
}

// NewSignal returns a Signal.
func NewSignal() *Signal { return &Signal{ch: make(chan struct{}, 1)} }

// Wake asks for a pass soon. It never blocks; wake-ups before the pass
// starts are coalesced into one. It implements schedules.Waker.
func (s *Signal) Wake() {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// Wake asks for a pass soon (a schedule was saved, a run was added, a job
// finished). It never blocks.
func (s *Scheduler) Wake() { s.signal.Wake() }

// Run passes (Tick) at once, then every Config.Tick and whenever woken,
// until ctx ends. Errors of a pass are logged; Run returns nil when ctx
// ends.
func (s *Scheduler) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "scheduler pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.signal.ch:
		}
	}
}

// Tick makes one pass: it fires the due schedules, then decides and
// launches the open runs. Errors of single schedules and runs are joined;
// they do not stop the pass. It is exported for tests and for callers that
// drive the scheduler themselves.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, fireErr := s.schedules.FireDue(ctx)
	if fireErr != nil {
		fireErr = fmt.Errorf("fire schedules: %w", fireErr)
	}
	return errors.Join(fireErr, s.launchOpen(ctx))
}
