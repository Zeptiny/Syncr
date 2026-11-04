package postgres

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CreateEnrollmentToken implements workers.TokenRepository.
func (s *Store) CreateEnrollmentToken(ctx context.Context, t workers.EnrollmentToken) error {
	err := s.q.CreateEnrollmentToken(ctx, db.CreateEnrollmentTokenParams{
		Hash:      t.Hash[:],
		CreatedAt: pgTime(t.CreatedAt),
		ExpiresAt: pgTime(t.ExpiresAt),
		UsedAt:    pgOptTime(t.UsedAt),
		UsedBy:    pgOptID(t.UsedBy),
	})
	return mapErr("create enrollment token", err)
}

// ConsumeEnrollmentToken implements workers.TokenRepository: a single UPDATE
// ... WHERE hash = $1 AND used_at IS NULL AND expires_at > $2 RETURNING. Of
// two concurrent redemptions the second waits for the first's row lock,
// re-evaluates used_at IS NULL, and matches nothing. An unknown, used or
// expired token all report ErrNotFound.
func (s *Store) ConsumeEnrollmentToken(ctx context.Context, hash workers.TokenHash, at time.Time, by domain.WorkerID) (workers.EnrollmentToken, error) {
	row, err := s.q.ConsumeEnrollmentToken(ctx, db.ConsumeEnrollmentTokenParams{
		Hash: hash[:], At: pgTime(at), UsedBy: pgID(by),
	})
	if err != nil {
		return workers.EnrollmentToken{}, mapErr("consume enrollment token", err)
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
