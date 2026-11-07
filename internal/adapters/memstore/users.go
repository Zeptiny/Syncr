package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// User, session and API token values hold no slices or maps, so copying
// the struct never aliases the store.

// CreateUser implements users.Repository.
func (s *Store) CreateUser(_ context.Context, u users.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createUserLocked(u)
}

func (s *Store) createUserLocked(u users.User) error {
	if _, ok := s.users[u.ID]; ok {
		return fmt.Errorf("user %s: %w", u.ID, domain.ErrAlreadyExists)
	}
	for _, other := range s.users {
		if other.Username == u.Username {
			return fmt.Errorf("username %q: %w", u.Username, domain.ErrAlreadyExists)
		}
	}
	s.users[u.ID] = u
	return nil
}

// CreateFirstUser implements users.Repository: the check and the insert
// happen under the store's lock, so of concurrent calls exactly one wins.
func (s *Store) CreateFirstUser(_ context.Context, u users.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.users) > 0 {
		return fmt.Errorf("create first user: a user exists: %w", domain.ErrFailedPrecondition)
	}
	return s.createUserLocked(u)
}

// CountUsers implements users.Repository.
func (s *Store) CountUsers(context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users), nil
}

// GetUser implements users.Repository.
func (s *Store) GetUser(_ context.Context, id domain.UserID) (users.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return users.User{}, fmt.Errorf("user %s: %w", id, domain.ErrNotFound)
	}
	return u, nil
}

// GetUserByUsername implements users.Repository.
func (s *Store) GetUserByUsername(_ context.Context, username string) (users.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Username == username {
			return u, nil
		}
	}
	return users.User{}, fmt.Errorf("username %q: %w", username, domain.ErrNotFound)
}

// ListUsers implements users.Repository.
func (s *Store) ListUsers(context.Context) ([]users.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]users.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b users.User) int { return cmp.Compare(a.Username, b.Username) })
	return out, nil
}

// UpdateUser implements users.Repository.
func (s *Store) UpdateUser(_ context.Context, u users.User) (users.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.users[u.ID]
	if !ok {
		return users.User{}, fmt.Errorf("user %s: %w", u.ID, domain.ErrNotFound)
	}
	if cur.Version != u.Version {
		return users.User{}, fmt.Errorf("user %s is at version %d, not %d: %w", u.ID, cur.Version, u.Version, domain.ErrConflict)
	}
	next := cur
	next.DisplayName, next.Role, next.PasswordHash = u.DisplayName, u.Role, u.PasswordHash
	next.DisabledAt, next.PasswordChangedAt, next.LastLoginAt, next.UpdatedAt = u.DisabledAt, u.PasswordChangedAt, u.LastLoginAt, u.UpdatedAt
	next.Version = cur.Version + 1
	s.users[u.ID] = next
	return next, nil
}

// CountActiveAdmins implements users.Repository.
func (s *Store) CountActiveAdmins(context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, u := range s.users {
		if u.Role == domain.RoleAdmin && !u.Disabled() {
			n++
		}
	}
	return n, nil
}

// DeleteUser implements users.Repository. Like the database's foreign keys,
// it refuses to delete a user who owns remotes (deleted ones included) or
// jobs, and removes the user's sessions and API tokens with the user.
func (s *Store) DeleteUser(_ context.Context, id domain.UserID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return fmt.Errorf("user %s: %w", id, domain.ErrNotFound)
	}
	for _, r := range s.remotes {
		if r.OwnerID == id {
			return fmt.Errorf("user %s owns remotes: %w", id, domain.ErrFailedPrecondition)
		}
	}
	for _, j := range s.jobs {
		if j.OwnerID == id {
			return fmt.Errorf("user %s owns jobs: %w", id, domain.ErrFailedPrecondition)
		}
	}
	for h, sess := range s.sessions {
		if sess.UserID == id {
			delete(s.sessions, h)
		}
	}
	for tid, t := range s.apiTokens {
		if t.UserID == id {
			delete(s.apiTokens, tid)
		}
	}
	delete(s.users, id)
	return nil
}

// CreateSession implements users.SessionRepository.
func (s *Store) CreateSession(_ context.Context, sess users.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess.IDHash]; ok {
		return fmt.Errorf("session: %w", domain.ErrAlreadyExists)
	}
	if _, ok := s.users[sess.UserID]; !ok {
		return fmt.Errorf("session of user %s: %w", sess.UserID, domain.ErrNotFound)
	}
	s.sessions[sess.IDHash] = sess
	return nil
}

// GetSession implements users.SessionRepository.
func (s *Store) GetSession(_ context.Context, h users.TokenHash) (users.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[h]
	if !ok {
		return users.Session{}, fmt.Errorf("session: %w", domain.ErrNotFound)
	}
	return sess, nil
}

// TouchSession implements users.SessionRepository.
func (s *Store) TouchSession(_ context.Context, h users.TokenHash, at, idleExpiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[h]
	if !ok {
		return fmt.Errorf("session: %w", domain.ErrNotFound)
	}
	sess.LastSeenAt, sess.IdleExpiresAt = at, idleExpiresAt
	s.sessions[h] = sess
	return nil
}

// DeleteSession implements users.SessionRepository.
func (s *Store) DeleteSession(_ context.Context, h users.TokenHash) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, h)
	return nil
}

// DeleteUserSessions implements users.SessionRepository.
func (s *Store) DeleteUserSessions(_ context.Context, user domain.UserID, keep *users.TokenHash) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for h, sess := range s.sessions {
		if sess.UserID == user && (keep == nil || h != *keep) {
			delete(s.sessions, h)
			n++
		}
	}
	return n, nil
}

// DeleteExpiredSessions implements users.SessionRepository.
func (s *Store) DeleteExpiredSessions(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for h, sess := range s.sessions {
		if sess.Expired(now) {
			delete(s.sessions, h)
			n++
		}
	}
	return n, nil
}

// CreateAPIToken implements users.APITokenRepository.
func (s *Store) CreateAPIToken(_ context.Context, t users.APIToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apiTokens[t.ID]; ok {
		return fmt.Errorf("api token %s: %w", t.ID, domain.ErrAlreadyExists)
	}
	for _, other := range s.apiTokens {
		if other.Hash == t.Hash {
			return fmt.Errorf("api token hash: %w", domain.ErrAlreadyExists)
		}
	}
	if _, ok := s.users[t.UserID]; !ok {
		return fmt.Errorf("api token of user %s: %w", t.UserID, domain.ErrNotFound)
	}
	s.apiTokens[t.ID] = t
	return nil
}

// GetAPITokenByHash implements users.APITokenRepository.
func (s *Store) GetAPITokenByHash(_ context.Context, h users.TokenHash) (users.APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.apiTokens {
		if t.Hash == h {
			return t, nil
		}
	}
	return users.APIToken{}, fmt.Errorf("api token: %w", domain.ErrNotFound)
}

// ListAPITokens implements users.APITokenRepository.
func (s *Store) ListAPITokens(_ context.Context, user domain.UserID) ([]users.APIToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []users.APIToken{}
	for _, t := range s.apiTokens {
		if t.UserID == user {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b users.APIToken) int { return compareIDs(b.ID, a.ID) })
	return out, nil
}

// DeleteAPIToken implements users.APITokenRepository.
func (s *Store) DeleteAPIToken(_ context.Context, user domain.UserID, id domain.APITokenID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.apiTokens[id]; !ok || t.UserID != user {
		return fmt.Errorf("api token %s: %w", id, domain.ErrNotFound)
	}
	delete(s.apiTokens, id)
	return nil
}

// TouchAPIToken implements users.APITokenRepository.
func (s *Store) TouchAPIToken(_ context.Context, id domain.APITokenID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.apiTokens[id]
	if !ok {
		return fmt.Errorf("api token %s: %w", id, domain.ErrNotFound)
	}
	t.LastUsedAt = at
	s.apiTokens[id] = t
	return nil
}
