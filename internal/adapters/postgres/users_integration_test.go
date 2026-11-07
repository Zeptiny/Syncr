//go:build integration

package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestDeleteUserOwningARemote checks the foreign key mapping of DeleteUser
// against a remote row (the storetest suite needs the remotes adapter for
// it): a referenced user is ErrFailedPrecondition, while foreign key
// violations elsewhere keep mapErr's meaning (a missing referenced row is
// ErrNotFound).
func TestDeleteUserOwningARemote(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := t.Context()
	owner := seedUser(t, s)
	if _, err := s.pool.Exec(ctx, `INSERT INTO remotes (id, owner_id, name, backend_type, created_at, updated_at, deleted_at)
		VALUES ($1, $2, 'Photos', 's3', now(), now(), now())`, pgID(domain.NewRemoteID()), pgID(owner)); err != nil {
		t.Fatal(err)
	}
	err := s.DeleteUser(ctx, owner)
	if !errors.Is(err, domain.ErrFailedPrecondition) || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DeleteUser(owner of a deleted remote) = %v, want only ErrFailedPrecondition", err)
	}
	if _, err := s.GetUser(ctx, owner); err != nil {
		t.Errorf("the owner was deleted: %v", err)
	}

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	err = s.CreateSession(ctx, users.Session{
		IDHash: users.HashToken("x"), UserID: domain.NewUserID(), CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now, ExpiresAt: now,
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("CreateSession(unknown user) = %v, want ErrNotFound", err)
	}
}

// TestSessionTextIsCleaned: request metadata with NUL bytes or invalid
// UTF-8 is stored, not rejected (Postgres refuses both in text columns).
func TestSessionTextIsCleaned(t *testing.T) {
	t.Parallel()
	s := newMigratedStore(t)
	ctx := t.Context()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	sess := users.Session{
		IDHash: users.HashToken("y"), UserID: seedUser(t, s), CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(time.Hour), UserAgent: "agent\x00\xff", ClientIP: "192.0.2.1",
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, sess.IDHash)
	if err != nil || !strings.HasPrefix(got.UserAgent, "agent") || strings.ContainsRune(got.UserAgent, 0) {
		t.Errorf("GetSession = (%+v, %v)", got, err)
	}
}
