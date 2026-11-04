package dispatch

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// ErrStaleEpoch reports a request carrying an epoch the worker has already
// replaced: a delayed request of a previous worker process. Errors wrapping
// it also match domain.ErrFailedPrecondition.
var ErrStaleEpoch = errors.New("dispatch: request from a replaced worker process (stale epoch)")

// Config tunes the dispatcher. Zero fields take the DefaultConfig values.
type Config struct {
	// MaxWait caps a long poll (PollRequest.wait); it also applies when the
	// worker does not set wait. Stay below common proxy idle timeouts.
	MaxWait time.Duration
	// MaxCommands caps the commands returned at once.
	MaxCommands int
	// DispatchTimeout fails a job that stays queued this long (its worker
	// is offline or never accepts it).
	DispatchTimeout time.Duration
	// SweepInterval is how often Run checks for dispatch timeouts.
	SweepInterval time.Duration
	// Passive tunes PassiveLoop.
	Passive PassiveConfig
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		MaxWait:         25 * time.Second,
		MaxCommands:     64,
		DispatchTimeout: 15 * time.Minute,
		SweepInterval:   30 * time.Second,
		Passive:         DefaultPassiveConfig(),
	}
}

// Deps are the dispatcher's collaborators. All are required except Logger.
type Deps struct {
	Jobs    Jobs
	Workers Workers
	Clock   Clock
	// Hub must be the same Hub given to the job service as its Waker.
	Hub *Hub
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Service is the controller side of the worker link. Create it with New;
// safe for concurrent use. It keeps only reconstructible state in memory
// (per-worker sessions), so a new Service over the same stores resumes
// where the old one stopped.
type Service struct {
	jobs    Jobs
	workers Workers
	clock   Clock
	hub     *Hub
	log     *slog.Logger
	cfg     Config

	mu       sync.Mutex
	sessions map[domain.WorkerID]*session
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Jobs == nil || d.Workers == nil || d.Clock == nil || d.Hub == nil {
		return nil, errors.New("dispatch: missing dependency")
	}
	def := DefaultConfig()
	cfg.MaxWait = cmp.Or(cfg.MaxWait, def.MaxWait)
	cfg.MaxCommands = cmp.Or(cfg.MaxCommands, def.MaxCommands)
	cfg.DispatchTimeout = cmp.Or(cfg.DispatchTimeout, def.DispatchTimeout)
	cfg.SweepInterval = cmp.Or(cfg.SweepInterval, def.SweepInterval)
	cfg.Passive = cfg.Passive.withDefaults()
	if cfg.MaxWait < 0 || cfg.MaxCommands < 0 || cfg.DispatchTimeout < 0 || cfg.SweepInterval < 0 {
		return nil, fmt.Errorf("dispatch: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		jobs: d.Jobs, workers: d.Workers, clock: d.Clock, hub: d.Hub, log: log, cfg: cfg,
		sessions: make(map[domain.WorkerID]*session),
	}, nil
}

// MaxWait returns the cap on a long poll (Config.MaxWait). Transports use
// it to size the time a Poll may hold its connection.
func (s *Service) MaxWait() time.Duration { return s.cfg.MaxWait }

// session is the in-memory link state of one worker. It is derived from
// what the worker told this controller process and is rebuilt from the
// worker's next requests after a restart, so it is never persisted. mu
// serializes the handling of one worker's link calls in this process (the
// stores' conditional updates keep it safe beyond that).
type session struct {
	mu sync.Mutex
	// epoch is the worker epoch the fields below belong to.
	epoch string
	// cancelAcked holds job IDs whose CancelJob the worker acknowledged in
	// epoch; they are not sent again.
	cancelAcked map[string]bool
	// orphans are job IDs the latest snapshot listed that the controller
	// considers finished or does not know; each gets a CancelJob.
	orphans []string
	// saturated mirrors WorkerState.outbox_saturated, to log changes.
	saturated bool
}

func (s *session) reset(epoch string) {
	s.epoch = epoch
	s.cancelAcked = make(map[string]bool)
	s.orphans = nil
	s.saturated = false
}

func (s *Service) session(id domain.WorkerID) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		sess = &session{cancelAcked: make(map[string]bool)}
		s.sessions[id] = sess
	}
	return sess
}

// Hub wakes the command long polls (and passive loops) of a worker when its
// pending commands change. It implements jobs.Waker. Create it with NewHub;
// safe for concurrent use.
type Hub struct {
	mu    sync.Mutex
	chans map[domain.WorkerID]chan struct{}
}

// NewHub returns a Hub.
func NewHub() *Hub { return &Hub{chans: make(map[domain.WorkerID]chan struct{})} }

// Wake wakes everyone waiting for id's commands. It never blocks.
func (h *Hub) Wake(id domain.WorkerID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.chans[id]; ok {
		close(ch)
		delete(h.chans, id)
	}
}

// wait returns a channel closed by the next Wake(id). Take it before
// checking for commands, so a wake between the check and the wait is not
// lost.
func (h *Hub) wait(id domain.WorkerID) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.chans[id]
	if !ok {
		ch = make(chan struct{})
		h.chans[id] = ch
	}
	return ch
}
