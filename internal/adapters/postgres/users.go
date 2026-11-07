package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Compile-time checks that Store implements the users ports.
var (
	_ users.Repository         = (*Store)(nil)
	_ users.SessionRepository  = (*Store)(nil)
	_ users.APITokenRepository = (*Store)(nil)
)

// The users ports are stubs until Milestone 3 track A implements them with
// queries/users.sql; every method reports errors.ErrUnsupported.
func usersUnsupported(op string) error {
	return fmt.Errorf("postgres: %s: %w", op, errors.ErrUnsupported)
}

// CreateUser implements users.Repository (not yet: ErrUnsupported).
func (s *Store) CreateUser(context.Context, users.User) error {
	return usersUnsupported("create user")
}

// CreateFirstUser implements users.Repository (not yet: ErrUnsupported).
func (s *Store) CreateFirstUser(context.Context, users.User) error {
	return usersUnsupported("create first user")
}

// CountUsers implements users.Repository (not yet: ErrUnsupported).
func (s *Store) CountUsers(context.Context) (int, error) {
	return 0, usersUnsupported("count users")
}

// GetUser implements users.Repository (not yet: ErrUnsupported).
func (s *Store) GetUser(context.Context, domain.UserID) (users.User, error) {
	return users.User{}, usersUnsupported("get user")
}

// GetUserByUsername implements users.Repository (not yet: ErrUnsupported).
func (s *Store) GetUserByUsername(context.Context, string) (users.User, error) {
	return users.User{}, usersUnsupported("get user by username")
}

// ListUsers implements users.Repository (not yet: ErrUnsupported).
func (s *Store) ListUsers(context.Context) ([]users.User, error) {
	return nil, usersUnsupported("list users")
}

// UpdateUser implements users.Repository (not yet: ErrUnsupported).
func (s *Store) UpdateUser(context.Context, users.User) (users.User, error) {
	return users.User{}, usersUnsupported("update user")
}

// CountActiveAdmins implements users.Repository (not yet: ErrUnsupported).
func (s *Store) CountActiveAdmins(context.Context) (int, error) {
	return 0, usersUnsupported("count active admins")
}

// DeleteUser implements users.Repository (not yet: ErrUnsupported).
func (s *Store) DeleteUser(context.Context, domain.UserID) error {
	return usersUnsupported("delete user")
}

// CreateSession implements users.SessionRepository (not yet:
// ErrUnsupported).
func (s *Store) CreateSession(context.Context, users.Session) error {
	return usersUnsupported("create session")
}

// GetSession implements users.SessionRepository (not yet: ErrUnsupported).
func (s *Store) GetSession(context.Context, users.TokenHash) (users.Session, error) {
	return users.Session{}, usersUnsupported("get session")
}

// TouchSession implements users.SessionRepository (not yet:
// ErrUnsupported).
func (s *Store) TouchSession(context.Context, users.TokenHash, time.Time, time.Time) error {
	return usersUnsupported("touch session")
}

// DeleteSession implements users.SessionRepository (not yet:
// ErrUnsupported).
func (s *Store) DeleteSession(context.Context, users.TokenHash) error {
	return usersUnsupported("delete session")
}

// DeleteUserSessions implements users.SessionRepository (not yet:
// ErrUnsupported).
func (s *Store) DeleteUserSessions(context.Context, domain.UserID, *users.TokenHash) (int, error) {
	return 0, usersUnsupported("delete user sessions")
}

// DeleteExpiredSessions implements users.SessionRepository (not yet:
// ErrUnsupported).
func (s *Store) DeleteExpiredSessions(context.Context, time.Time) (int, error) {
	return 0, usersUnsupported("delete expired sessions")
}

// CreateAPIToken implements users.APITokenRepository (not yet:
// ErrUnsupported).
func (s *Store) CreateAPIToken(context.Context, users.APIToken) error {
	return usersUnsupported("create api token")
}

// GetAPITokenByHash implements users.APITokenRepository (not yet:
// ErrUnsupported).
func (s *Store) GetAPITokenByHash(context.Context, users.TokenHash) (users.APIToken, error) {
	return users.APIToken{}, usersUnsupported("get api token")
}

// ListAPITokens implements users.APITokenRepository (not yet:
// ErrUnsupported).
func (s *Store) ListAPITokens(context.Context, domain.UserID) ([]users.APIToken, error) {
	return nil, usersUnsupported("list api tokens")
}

// DeleteAPIToken implements users.APITokenRepository (not yet:
// ErrUnsupported).
func (s *Store) DeleteAPIToken(context.Context, domain.UserID, domain.APITokenID) error {
	return usersUnsupported("delete api token")
}

// TouchAPIToken implements users.APITokenRepository (not yet:
// ErrUnsupported).
func (s *Store) TouchAPIToken(context.Context, domain.APITokenID, time.Time) error {
	return usersUnsupported("touch api token")
}
