package memstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateWorker implements workers.Repository.
func (s *Store) CreateWorker(_ context.Context, w workers.Worker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workers[w.ID]; ok {
		return fmt.Errorf("worker %s: %w", w.ID, domain.ErrAlreadyExists)
	}
	s.workers[w.ID] = cloneWorker(w)
	return nil
}

// GetWorker implements workers.Repository.
func (s *Store) GetWorker(_ context.Context, id domain.WorkerID) (workers.Worker, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.workers[id]
	if !ok {
		return workers.Worker{}, fmt.Errorf("worker %s: %w", id, domain.ErrNotFound)
	}
	return cloneWorker(w), nil
}

// ListWorkers implements workers.Repository.
func (s *Store) ListWorkers(_ context.Context, f workers.ListFilter) ([]workers.Worker, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []workers.Worker
	for _, w := range s.workers {
		if (f.Mode == "" || w.Mode == f.Mode) && (f.IncludeRemoved || !w.Removed()) {
			out = append(out, cloneWorker(w))
		}
	}
	slices.SortFunc(out, func(a, b workers.Worker) int { return compareIDs(a.ID, b.ID) })
	return out, nil
}

// RenameWorker implements workers.Repository.
func (s *Store) RenameWorker(_ context.Context, id domain.WorkerID, name string) (workers.Worker, error) {
	return s.updateWorker(id, func(w *workers.Worker) error {
		w.Name = name
		return nil
	})
}

// RemoveWorker implements workers.Repository.
func (s *Store) RemoveWorker(_ context.Context, id domain.WorkerID, at time.Time) (workers.Worker, error) {
	return s.updateWorker(id, func(w *workers.Worker) error {
		w.RemovedAt = at
		w.SecretEnc = nil
		return nil
	})
}

// SetWorkerInfo implements workers.Repository.
func (s *Store) SetWorkerInfo(_ context.Context, id domain.WorkerID, epoch string, info workers.Info) error {
	_, err := s.updateWorker(id, func(w *workers.Worker) error {
		if w.Link.Epoch != epoch {
			return fmt.Errorf("worker %s is in epoch %q, not %q: %w", id, w.Link.Epoch, epoch, domain.ErrConflict)
		}
		w.Info = info
		return nil
	})
	return err
}

// AdvanceWorkerEpoch implements workers.Repository.
func (s *Store) AdvanceWorkerEpoch(_ context.Context, id domain.WorkerID, from string, to workers.NewEpoch) (workers.Worker, error) {
	return s.updateWorker(id, func(w *workers.Worker) error {
		if w.Link.Epoch != from {
			return fmt.Errorf("worker %s is in epoch %q, not %q: %w", id, w.Link.Epoch, from, domain.ErrConflict)
		}
		w.Link.Epoch = to.Epoch
		w.Link.EpochStartedAt = to.StartedAt
		w.Link.PastEpochs = slices.Clone(to.PastEpochs)
		w.Link.AckSeq = 0
		return nil
	})
}

// AdvanceWorkerAckSeq implements workers.Repository.
func (s *Store) AdvanceWorkerAckSeq(_ context.Context, id domain.WorkerID, epoch string, seq int64) (int64, error) {
	w, err := s.updateWorker(id, func(w *workers.Worker) error {
		if w.Link.Epoch != epoch {
			return fmt.Errorf("worker %s is in epoch %q, not %q: %w", id, w.Link.Epoch, epoch, domain.ErrConflict)
		}
		w.Link.AckSeq = max(w.Link.AckSeq, seq)
		return nil
	})
	return w.Link.AckSeq, err
}

// TouchWorker implements workers.Repository.
func (s *Store) TouchWorker(_ context.Context, id domain.WorkerID, at time.Time) error {
	_, err := s.updateWorker(id, func(w *workers.Worker) error {
		if at.After(w.Link.LastSeen) {
			w.Link.LastSeen = at
		}
		return nil
	})
	return err
}

// updateWorker applies fn to a worker that exists and is not removed.
func (s *Store) updateWorker(id domain.WorkerID, fn func(*workers.Worker) error) (workers.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workers[id]
	if !ok || w.Removed() {
		return workers.Worker{}, fmt.Errorf("worker %s: %w", id, domain.ErrNotFound)
	}
	w = cloneWorker(w)
	if err := fn(&w); err != nil {
		return workers.Worker{}, err
	}
	s.workers[id] = w
	return cloneWorker(w), nil
}

// CreateEnrollmentToken implements workers.TokenRepository.
func (s *Store) CreateEnrollmentToken(_ context.Context, t workers.EnrollmentToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[t.Hash]; ok {
		return fmt.Errorf("enrollment token: %w", domain.ErrAlreadyExists)
	}
	s.tokens[t.Hash] = t
	return nil
}

// ConsumeEnrollmentToken implements workers.TokenRepository.
func (s *Store) ConsumeEnrollmentToken(_ context.Context, hash workers.TokenHash, at time.Time, by domain.WorkerID) (workers.EnrollmentToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok || !t.UsedAt.IsZero() || !at.Before(t.ExpiresAt) {
		return workers.EnrollmentToken{}, fmt.Errorf("enrollment token: %w", domain.ErrNotFound)
	}
	t.UsedAt, t.UsedBy = at, by
	s.tokens[hash] = t
	return t, nil
}

// DeleteEnrollmentTokens implements workers.TokenRepository.
func (s *Store) DeleteEnrollmentTokens(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for h, t := range s.tokens {
		if t.ExpiresAt.Before(before) {
			delete(s.tokens, h)
			n++
		}
	}
	return n, nil
}
