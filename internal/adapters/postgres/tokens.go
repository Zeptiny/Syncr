package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateEnrollmentToken implements workers.TokenRepository.
func (s *Store) CreateEnrollmentToken(ctx context.Context, t workers.EnrollmentToken) error {
	err := s.q.CreateEnrollmentToken(ctx, db.CreateEnrollmentTokenParams{
		ID:        pgOptID(t.ID), // a zero ID violates NOT NULL
		Hash:      t.Hash[:],
		CreatedAt: pgTime(t.CreatedAt),
		ExpiresAt: pgTime(t.ExpiresAt),
		UsedAt:    pgOptTime(t.UsedAt),
		UsedBy:    pgOptID(t.UsedBy),
	})
	return mapErr("create enrollment token", err)
}

// GetEnrollmentToken implements workers.TokenRepository.
func (s *Store) GetEnrollmentToken(ctx context.Context, id domain.EnrollmentTokenID) (workers.EnrollmentToken, error) {
	row, err := s.q.GetEnrollmentToken(ctx, pgID(id))
	if err != nil {
		return workers.EnrollmentToken{}, mapErr(fmt.Sprintf("get enrollment token %s", id), err)
	}
	return tokenFrom(row)
}

// EnrollWorker implements workers.TokenRepository: one transaction of the
// redeeming UPDATE ... WHERE hash = $1 AND used_at IS NULL AND expires_at >
// $2 RETURNING and the worker INSERT. Of two concurrent redemptions the
// second waits for the first's row lock and re-evaluates used_at IS NULL:
// it matches nothing if the first committed, and redeems the token if the
// first rolled back (because its insert failed). An unknown, used or
// expired token all report ErrNotFound, before the insert is tried.
func (s *Store) EnrollWorker(ctx context.Context, hash workers.TokenHash, at time.Time, w workers.Worker) (workers.EnrollmentToken, error) {
	p, err := createWorkerParams(w)
	if err != nil {
		return workers.EnrollmentToken{}, err
	}
	p.Labels = textArray(w.Labels)
	var row db.EnrollmentToken
	err = s.inTx(ctx, fmt.Sprintf("enroll worker %s", w.ID), func(q *db.Queries) error {
		var err error
		row, err = q.ConsumeEnrollmentToken(ctx, db.ConsumeEnrollmentTokenParams{
			Hash: hash[:], At: pgTime(at), UsedBy: pgID(w.ID),
		})
		if err != nil {
			return mapErr("consume enrollment token", err)
		}
		return mapErr(fmt.Sprintf("create worker %s", w.ID), q.CreateWorker(ctx, p))
	})
	if err != nil {
		return workers.EnrollmentToken{}, err
	}
	return tokenFrom(row)
}

// DeleteEnrollmentTokens implements workers.TokenRepository.
func (s *Store) DeleteEnrollmentTokens(ctx context.Context, before time.Time) (int, error) {
	n, err := s.q.DeleteEnrollmentTokens(ctx, pgTime(before))
	if err != nil {
		return 0, mapErr("delete enrollment tokens", err)
	}
	return int(n), nil
}
