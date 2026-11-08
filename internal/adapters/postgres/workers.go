package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateWorker implements workers.Repository. A live worker whose name is
// taken (case-insensitively) violates workers_name_key: ErrAlreadyExists.
func (s *Store) CreateWorker(ctx context.Context, w workers.Worker) error {
	p, err := createWorkerParams(w)
	if err != nil {
		return err
	}
	p.Labels = textArray(w.Labels)
	if err := s.q.CreateWorker(ctx, p); err != nil {
		return mapErr(fmt.Sprintf("create worker %s", w.ID), err)
	}
	return nil
}

// GetWorker implements workers.Repository.
func (s *Store) GetWorker(ctx context.Context, id domain.WorkerID) (workers.Worker, error) {
	row, err := s.q.GetWorker(ctx, pgID(id))
	if err != nil {
		return workers.Worker{}, mapErr(fmt.Sprintf("get worker %s", id), err)
	}
	return workerFrom(row)
}

// ListWorkers implements workers.Repository.
func (s *Store) ListWorkers(ctx context.Context, f workers.ListFilter) ([]workers.Worker, error) {
	rows, err := s.q.ListWorkers(ctx, db.ListWorkersParams{Mode: string(f.Mode), IncludeRemoved: f.IncludeRemoved, Label: f.Label})
	if err != nil {
		return nil, mapErr("list workers", err)
	}
	return workersFrom(rows)
}

// RenameWorker implements workers.Repository. A name taken by another live
// worker violates workers_name_key: ErrAlreadyExists.
func (s *Store) RenameWorker(ctx context.Context, id domain.WorkerID, name string) (workers.Worker, error) {
	row, err := s.q.RenameWorker(ctx, db.RenameWorkerParams{ID: pgID(id), Name: name})
	if err != nil {
		return workers.Worker{}, mapErr(fmt.Sprintf("rename worker %s", id), err)
	}
	return workerFrom(row)
}

// SetWorkerLabels implements workers.Repository.
func (s *Store) SetWorkerLabels(ctx context.Context, id domain.WorkerID, labels []string) (workers.Worker, error) {
	row, err := s.q.SetWorkerLabels(ctx, db.SetWorkerLabelsParams{ID: pgID(id), Labels: textArray(labels)})
	if err != nil {
		return workers.Worker{}, mapErr(fmt.Sprintf("set labels of worker %s", id), err)
	}
	return workerFrom(row)
}

// RemoveWorker implements workers.Repository.
func (s *Store) RemoveWorker(ctx context.Context, id domain.WorkerID, at time.Time) (workers.Worker, error) {
	row, err := s.q.RemoveWorker(ctx, db.RemoveWorkerParams{ID: pgID(id), RemovedAt: pgTime(at)})
	if err != nil {
		return workers.Worker{}, mapErr(fmt.Sprintf("remove worker %s", id), err)
	}
	return workerFrom(row)
}

// SetWorkerInfo implements workers.Repository: one UPDATE conditional on the
// epoch.
func (s *Store) SetWorkerInfo(ctx context.Context, id domain.WorkerID, epoch string, info workers.Info) error {
	doc, err := encodeInfo(info)
	if err != nil {
		return err
	}
	op := fmt.Sprintf("set info of worker %s", id)
	n, err := s.q.SetWorkerInfo(ctx, db.SetWorkerInfoParams{ID: pgID(id), Epoch: epoch, Info: doc})
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return s.casFailed(ctx, op, id, epoch)
	}
	return nil
}

// AdvanceWorkerEpoch implements workers.Repository: a compare-and-swap on the
// epoch in a single UPDATE ... WHERE epoch = from RETURNING.
func (s *Store) AdvanceWorkerEpoch(ctx context.Context, id domain.WorkerID, from string, to workers.NewEpoch) (workers.Worker, error) {
	op := fmt.Sprintf("advance epoch of worker %s", id)
	row, err := s.q.AdvanceWorkerEpoch(ctx, db.AdvanceWorkerEpochParams{
		ID:         pgID(id),
		FromEpoch:  from,
		ToEpoch:    to.Epoch,
		StartedAt:  pgOptTime(to.StartedAt),
		PastEpochs: textArray(to.PastEpochs),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return workers.Worker{}, s.casFailed(ctx, op, id, from)
	}
	if err != nil {
		return workers.Worker{}, mapErr(op, err)
	}
	return workerFrom(row)
}

// AdvanceWorkerAckSeq implements workers.Repository: ack_seq =
// GREATEST(ack_seq, seq) in one UPDATE, so concurrent calls can only move the
// cursor forward.
func (s *Store) AdvanceWorkerAckSeq(ctx context.Context, id domain.WorkerID, epoch string, seq int64) (int64, error) {
	op := fmt.Sprintf("advance ack seq of worker %s", id)
	got, err := s.q.AdvanceWorkerAckSeq(ctx, db.AdvanceWorkerAckSeqParams{ID: pgID(id), Epoch: epoch, Seq: seq})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, s.casFailed(ctx, op, id, epoch)
	}
	if err != nil {
		return 0, mapErr(op, err)
	}
	return got, nil
}

// TouchWorker implements workers.Repository: last_seen =
// GREATEST(last_seen, at).
func (s *Store) TouchWorker(ctx context.Context, id domain.WorkerID, at time.Time) error {
	op := fmt.Sprintf("touch worker %s", id)
	n, err := s.q.TouchWorker(ctx, db.TouchWorkerParams{ID: pgID(id), At: pgTime(at)})
	if err != nil {
		return mapErr(op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return nil
}

// casFailed explains a conditional worker update that matched no row: the
// worker is missing or removed (ErrNotFound), or it exists in another epoch
// (ErrConflict). The second query is not atomic with the first, which is
// harmless: the outcome is only a hint for the caller, who re-reads either
// way.
func (s *Store) casFailed(ctx context.Context, op string, id domain.WorkerID, epoch string) error {
	active, err := s.q.WorkerIsActive(ctx, pgID(id))
	if err != nil {
		return mapErr(op, err)
	}
	if !active {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return fmt.Errorf("%s: worker is not in epoch %q: %w", op, epoch, domain.ErrConflict)
}
