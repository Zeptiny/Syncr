package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Compile-time check that Store implements the remotes port.
var _ remotes.Repository = (*Store)(nil)

// The remotes port is a stub until Milestone 3 track B implements it with
// queries/remotes.sql; every method reports errors.ErrUnsupported.
func remotesUnsupported(op string) error {
	return fmt.Errorf("postgres: %s: %w", op, errors.ErrUnsupported)
}

// CreateRemote implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) CreateRemote(context.Context, remotes.Remote) error {
	return remotesUnsupported("create remote")
}

// GetRemote implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) GetRemote(context.Context, domain.RemoteID) (remotes.Remote, error) {
	return remotes.Remote{}, remotesUnsupported("get remote")
}

// GetRemotes implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) GetRemotes(context.Context, []domain.RemoteID) ([]remotes.Remote, error) {
	return nil, remotesUnsupported("get remotes")
}

// ListRemotes implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) ListRemotes(context.Context, remotes.ListFilter) ([]remotes.Remote, error) {
	return nil, remotesUnsupported("list remotes")
}

// UpdateRemote implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) UpdateRemote(context.Context, remotes.Remote) (remotes.Remote, error) {
	return remotes.Remote{}, remotesUnsupported("update remote")
}

// DeleteRemote implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) DeleteRemote(context.Context, domain.RemoteID, time.Time) error {
	return remotesUnsupported("delete remote")
}

// ListUnionsUsing implements remotes.Repository (not yet: ErrUnsupported).
func (s *Store) ListUnionsUsing(context.Context, domain.RemoteID) ([]remotes.Remote, error) {
	return nil, remotesUnsupported("list unions using a remote")
}
