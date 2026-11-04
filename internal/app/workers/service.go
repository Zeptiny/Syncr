package workers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// OfflineAfter is how long after the last contact a worker counts as
	// offline. Active workers heartbeat about every 15s and passive ones are
	// pulled as often, so the default allows two missed beats.
	OfflineAfter time.Duration
	// TokenTTL is the default enrollment token lifetime.
	TokenTTL time.Duration
	// MaxTokenTTL caps requested token lifetimes.
	MaxTokenTTL time.Duration
}

// MinTokenTTL is the shortest enrollment token lifetime accepted.
const MinTokenTTL = time.Minute

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		OfflineAfter: 45 * time.Second,
		TokenTTL:     time.Hour,
		MaxTokenTTL:  7 * 24 * time.Hour,
	}
}

// Deps are the service's collaborators. All are required except Logger.
type Deps struct {
	Workers Repository
	Tokens  TokenRepository
	Cipher  Cipher
	Clock   Clock
	IDs     IDGenerator
	Jobs    JobReleaser
	// Random is the source of tokens and secrets: crypto/rand.Reader in
	// production.
	Random io.Reader
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Service implements the worker use cases. It is stateless apart from its
// dependencies and safe for concurrent use.
type Service struct {
	repo   Repository
	tokens TokenRepository
	cipher Cipher
	clock  Clock
	ids    IDGenerator
	jobs   JobReleaser
	random io.Reader
	log    *slog.Logger
	cfg    Config
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Workers == nil || d.Tokens == nil || d.Cipher == nil || d.Clock == nil || d.IDs == nil || d.Jobs == nil || d.Random == nil {
		return nil, errors.New("workers: missing dependency")
	}
	def := DefaultConfig()
	cfg.OfflineAfter = cmp.Or(cfg.OfflineAfter, def.OfflineAfter)
	cfg.TokenTTL = cmp.Or(cfg.TokenTTL, def.TokenTTL)
	cfg.MaxTokenTTL = cmp.Or(cfg.MaxTokenTTL, def.MaxTokenTTL)
	if cfg.OfflineAfter < 0 || cfg.TokenTTL < MinTokenTTL || cfg.MaxTokenTTL < cfg.TokenTTL {
		return nil, fmt.Errorf("workers: invalid config %+v", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Workers, tokens: d.Tokens, cipher: d.Cipher, clock: d.Clock,
		ids: d.IDs, jobs: d.Jobs, random: d.Random, log: log, cfg: cfg,
	}, nil
}

// Status derives w's connection status at the current time.
func (s *Service) Status(w Worker) domain.WorkerStatus {
	return domain.DeriveWorkerStatus(w.Link.LastSeen, s.clock.Now(), s.cfg.OfflineAfter)
}

// Get returns a worker that has not been removed. ErrNotFound otherwise.
func (s *Service) Get(ctx context.Context, id domain.WorkerID) (Worker, error) {
	if id.IsZero() {
		return Worker{}, fmt.Errorf("get worker: %w", domain.ErrInvalidID)
	}
	w, err := s.repo.GetWorker(ctx, id)
	if err != nil {
		return Worker{}, fmt.Errorf("get worker %s: %w", id, err)
	}
	if w.Removed() {
		return Worker{}, fmt.Errorf("get worker %s: %w", id, domain.ErrNotFound)
	}
	return w, nil
}

// List returns the workers matching f, sorted by name (case-insensitive),
// then ID.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Worker, error) {
	if f.Mode != "" && !f.Mode.Valid() {
		return nil, fmt.Errorf("list workers: %w: mode %q", domain.ErrInvalidArgument, f.Mode)
	}
	ws, err := s.repo.ListWorkers(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	slices.SortFunc(ws, func(a, b Worker) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID.String(), b.ID.String()))
	})
	return ws, nil
}

// Rename sets a worker's display name.
func (s *Service) Rename(ctx context.Context, id domain.WorkerID, name string) (Worker, error) {
	if id.IsZero() {
		return Worker{}, fmt.Errorf("rename worker: %w", domain.ErrInvalidID)
	}
	name, err := validName(name)
	if err != nil {
		return Worker{}, fmt.Errorf("rename worker %s: %w", id, err)
	}
	w, err := s.repo.RenameWorker(ctx, id, name)
	if err != nil {
		return Worker{}, fmt.Errorf("rename worker %s: %w", id, err)
	}
	return w, nil
}

// Remove removes a worker (see the package doc). It is idempotent: removing
// a removed worker only ends its remaining jobs again. ErrNotFound if the
// worker never existed.
func (s *Service) Remove(ctx context.Context, id domain.WorkerID) error {
	if id.IsZero() {
		return fmt.Errorf("remove worker: %w", domain.ErrInvalidID)
	}
	w, err := s.repo.GetWorker(ctx, id)
	if err != nil {
		return fmt.Errorf("remove worker %s: %w", id, err)
	}
	if !w.Removed() {
		if _, err := s.repo.RemoveWorker(ctx, id, s.clock.Now()); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("remove worker %s: %w", id, err)
		}
		s.log.InfoContext(ctx, "worker removed", "worker_id", id.String(), "name", w.Name)
	}
	if err := s.jobs.ReleaseWorkerJobs(ctx, id); err != nil {
		return fmt.Errorf("remove worker %s: release jobs: %w", id, err)
	}
	return nil
}
