package passive

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Defaults for SupervisorOptions.
const (
	DefaultResync     = 30 * time.Second
	DefaultMinRestart = time.Second
	DefaultMaxRestart = time.Minute
)

// Targets is the part of *workers.Service the supervisor uses.
type Targets interface {
	List(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error)
	PassiveTarget(ctx context.Context, id domain.WorkerID) (workers.PassiveTarget, error)
}

// Loop is the link loop of one worker; *dispatch.PassiveLoop implements it.
// Run runs until ctx ends; returning earlier (or panicking) counts as a
// crash.
type Loop interface {
	Run(ctx context.Context) error
}

// NewLoopFunc creates the loop of a worker talking through client.
type NewLoopFunc func(id domain.WorkerID, client dispatch.PassiveClient) Loop

// DispatchLoops returns the NewLoopFunc of the dispatcher's PassiveLoops.
func DispatchLoops(svc *dispatch.Service) NewLoopFunc {
	return func(id domain.WorkerID, client dispatch.PassiveClient) Loop { return svc.NewPassiveLoop(id, client) }
}

// SupervisorOptions configure a Supervisor. Workers and NewLoop are
// required; zero values elsewhere select the defaults.
type SupervisorOptions struct {
	Workers Targets
	NewLoop NewLoopFunc
	// Client configures the clients of the loops.
	Client ClientConfig
	// Resync is how often the passive workers are listed again.
	Resync time.Duration
	// Wake, if set, triggers an immediate resync (e.g. after an admin added,
	// changed or removed a passive worker). Sends must not block the
	// sender: use a buffered channel and a non-blocking send.
	Wake <-chan struct{}
	// MinRestart and MaxRestart bound the exponential backoff before a
	// crashed loop is restarted.
	MinRestart, MaxRestart time.Duration
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Supervisor keeps one loop running per passive worker. Create it with
// NewSupervisor and call Run once.
type Supervisor struct {
	targets    Targets
	newLoop    NewLoopFunc
	client     ClientConfig
	resync     time.Duration
	wake       <-chan struct{}
	minRestart time.Duration
	maxRestart time.Duration
	log        *slog.Logger

	mu    sync.Mutex
	loops map[domain.WorkerID]*managed
	// failed remembers the target key whose client could not be built, to
	// log each problem once.
	failed map[domain.WorkerID]string
}

// managed is a running loop.
type managed struct {
	key    string
	client *Client
	cancel context.CancelFunc
	done   chan struct{}
}

// NewSupervisor returns a Supervisor.
func NewSupervisor(opts SupervisorOptions) (*Supervisor, error) {
	if opts.Workers == nil || opts.NewLoop == nil {
		return nil, errors.New("passive supervisor: missing dependency")
	}
	if opts.Resync < 0 || opts.MinRestart < 0 || opts.MaxRestart < 0 {
		return nil, errors.New("passive supervisor: negative interval")
	}
	s := &Supervisor{
		targets:    opts.Workers,
		newLoop:    opts.NewLoop,
		client:     opts.Client,
		resync:     cmp.Or(opts.Resync, DefaultResync),
		wake:       opts.Wake,
		minRestart: cmp.Or(opts.MinRestart, DefaultMinRestart),
		log:        opts.Logger,
		loops:      make(map[domain.WorkerID]*managed),
		failed:     make(map[domain.WorkerID]string),
	}
	s.maxRestart = max(cmp.Or(opts.MaxRestart, DefaultMaxRestart), s.minRestart)
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Run reconciles the loops with the registered passive workers until ctx
// ends, then stops every loop, waits for them and returns nil.
func (s *Supervisor) Run(ctx context.Context) error {
	t := time.NewTicker(s.resync)
	defer t.Stop()
	defer s.stopAll()
	for {
		s.sync(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.wake:
		}
	}
}

// Running returns the IDs of the workers whose loop runs, sorted.
func (s *Supervisor) Running() []domain.WorkerID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.SortedFunc(maps.Keys(s.loops), func(a, b domain.WorkerID) int { return cmp.Compare(a.String(), b.String()) })
}

// sync starts, restarts and stops loops to match the worker list. If the
// list cannot be read, the running loops are left alone.
func (s *Supervisor) sync(ctx context.Context) {
	ws, err := s.targets.List(ctx, workers.ListFilter{Mode: domain.WorkerPassive})
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "cannot list passive workers", "error", err.Error())
		}
		return
	}
	listed := make(map[domain.WorkerID]bool, len(ws))
	for _, w := range ws {
		listed[w.ID] = true
		key := targetKey(w)
		s.mu.Lock()
		m, running := s.loops[w.ID]
		s.mu.Unlock()
		if running && m.key == key {
			continue
		}
		if running {
			s.log.InfoContext(ctx, "passive worker changed; restarting its link", "worker_id", w.ID.String())
			s.stop(w.ID, m)
		}
		if err := s.start(ctx, w.ID, key); err != nil {
			if s.failed[w.ID] != key && ctx.Err() == nil {
				s.log.ErrorContext(ctx, "cannot start the link of a passive worker", "worker_id", w.ID.String(), "error", err.Error())
			}
			s.failed[w.ID] = key
			continue
		}
		delete(s.failed, w.ID)
	}
	s.mu.Lock()
	gone := make(map[domain.WorkerID]*managed)
	for id, m := range s.loops {
		if !listed[id] {
			gone[id] = m
		}
	}
	s.mu.Unlock()
	for id, m := range gone {
		s.log.InfoContext(ctx, "passive worker removed; stopping its link", "worker_id", id.String())
		s.stop(id, m)
	}
	for id := range s.failed {
		if !listed[id] {
			delete(s.failed, id)
		}
	}
}

// start builds the worker's client and loop and runs it.
func (s *Supervisor) start(ctx context.Context, id domain.WorkerID, key string) error {
	t, err := s.targets.PassiveTarget(ctx, id)
	if err != nil {
		return err
	}
	c, err := NewClient(t, s.client)
	if err != nil {
		return err
	}
	loop := s.newLoop(id, c)
	loopCtx, cancel := context.WithCancel(ctx)
	m := &managed{key: key, client: c, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.loops[id] = m
	s.mu.Unlock()
	go s.supervise(loopCtx, id, loop, m.done)
	s.log.InfoContext(ctx, "passive worker link started", "worker_id", id.String(), "url", t.URL)
	return nil
}

// stop cancels a loop and waits for it.
func (s *Supervisor) stop(id domain.WorkerID, m *managed) {
	m.cancel()
	<-m.done
	m.client.Close()
	s.mu.Lock()
	if s.loops[id] == m {
		delete(s.loops, id)
	}
	s.mu.Unlock()
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	all := maps.Clone(s.loops)
	s.mu.Unlock()
	for _, m := range all {
		m.cancel()
	}
	for id, m := range all {
		s.stop(id, m)
	}
}

// supervise runs loop until ctx ends, restarting it with backoff whenever
// it returns or panics early.
func (s *Supervisor) supervise(ctx context.Context, id domain.WorkerID, loop Loop, done chan<- struct{}) {
	defer close(done)
	var backoff time.Duration
	for {
		started := time.Now()
		err := runLoop(ctx, loop)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= s.maxRestart {
			backoff = 0 // it ran fine for a while
		}
		backoff = min(max(2*backoff, s.minRestart), s.maxRestart)
		if err == nil {
			err = errors.New("loop returned")
		}
		s.log.ErrorContext(ctx, "passive worker link stopped unexpectedly; restarting", "worker_id", id.String(),
			"retry_in", backoff.String(), "error", err.Error())
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// runLoop runs loop, turning a panic into an error.
func runLoop(ctx context.Context, loop Loop) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("loop panicked: %v\n%s", v, debug.Stack())
		}
	}()
	return loop.Run(ctx)
}

// targetKey identifies what a worker's client is built from: its URL,
// fingerprint and (encrypted) secret. A change of any of them restarts the
// loop with a new client.
func targetKey(w workers.Worker) string {
	sum := sha256.Sum256(w.SecretEnc)
	return w.URL + "\n" + w.CertFingerprint + "\n" + hex.EncodeToString(sum[:])
}
