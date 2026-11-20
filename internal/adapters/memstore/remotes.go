package memstore

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func cloneRemote(r remotes.Remote) remotes.Remote {
	r.Options = maps.Clone(r.Options)
	r.SecretsEnc = slices.Clone(r.SecretsEnc)
	r.SecretKeys = slices.Clone(r.SecretKeys)
	r.Members = slices.Clone(r.Members) // Member holds no references
	return r
}

// compareRemotes orders remotes by lower-cased name, then ID.
func compareRemotes(a, b remotes.Remote) int {
	return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), compareIDs(a.ID, b.ID))
}

// checkRemoteLocked enforces what the database's constraints (and the
// locks of its transactions) enforce for a remote r that is about to be
// stored: the name's length, case-insensitive name uniqueness among the
// owner's live remotes, members that exist and are live, are not the
// remote itself and have a known mode, and a wrapped remote that exists,
// is live and is not the remote itself, without a wrapped path when there
// is no wrapped remote.
func (s *Store) checkRemoteLocked(r remotes.Remote) error {
	if n := utf8.RuneCountInString(r.Name); n < 1 || n > 64 {
		return fmt.Errorf("remote name: %w: must be 1 to 64 characters", domain.ErrInvalidArgument)
	}
	lower := strings.ToLower(r.Name)
	for _, other := range s.remotes {
		if other.ID != r.ID && other.OwnerID == r.OwnerID && !other.Deleted() && strings.ToLower(other.Name) == lower {
			return fmt.Errorf("remote name %q: %w", r.Name, domain.ErrAlreadyExists)
		}
	}
	for i, m := range r.Members {
		switch member, ok := s.remotes[m.RemoteID]; {
		case m.RemoteID == r.ID:
			return fmt.Errorf("member %d: %w: a union cannot contain itself", i, domain.ErrInvalidArgument)
		case !ok || member.Deleted():
			return fmt.Errorf("member %d: remote %s: %w", i, m.RemoteID, domain.ErrNotFound)
		case !m.Mode.Valid():
			return fmt.Errorf("member %d: %w: mode %q", i, domain.ErrInvalidArgument, m.Mode)
		}
	}
	switch w := r.Wrapped; {
	case w.RemoteID.IsZero() && w.Path != "":
		return fmt.Errorf("wrapped remote: %w: a path without a remote", domain.ErrInvalidArgument)
	case w.RemoteID.IsZero():
	case w.RemoteID == r.ID:
		return fmt.Errorf("wrapped remote: %w: a remote cannot store its data in itself", domain.ErrInvalidArgument)
	default:
		if wrapped, ok := s.remotes[w.RemoteID]; !ok || wrapped.Deleted() {
			return fmt.Errorf("wrapped remote %s: %w", w.RemoteID, domain.ErrNotFound)
		}
	}
	return nil
}

// CreateRemote implements remotes.Repository.
func (s *Store) CreateRemote(_ context.Context, r remotes.Remote) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.remotes[r.ID]; ok {
		return fmt.Errorf("remote %s: %w", r.ID, domain.ErrAlreadyExists)
	}
	if _, ok := s.users[r.OwnerID]; !ok {
		return fmt.Errorf("owner %s of remote %s: %w", r.OwnerID, r.ID, domain.ErrNotFound)
	}
	if err := s.checkRemoteLocked(r); err != nil {
		return fmt.Errorf("create remote %s: %w", r.ID, err)
	}
	s.remotes[r.ID] = cloneRemote(r)
	return nil
}

// GetRemote implements remotes.Repository.
func (s *Store) GetRemote(_ context.Context, id domain.RemoteID) (remotes.Remote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.remotes[id]
	if !ok {
		return remotes.Remote{}, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
	}
	return cloneRemote(r), nil
}

// GetRemotes implements remotes.Repository.
func (s *Store) GetRemotes(_ context.Context, ids []domain.RemoteID) ([]remotes.Remote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[domain.RemoteID]bool, len(ids))
	out := []remotes.Remote{}
	for _, id := range ids {
		if r, ok := s.remotes[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, cloneRemote(r))
		}
	}
	slices.SortFunc(out, func(a, b remotes.Remote) int { return compareIDs(a.ID, b.ID) })
	return out, nil
}

// ListRemotes implements remotes.Repository.
func (s *Store) ListRemotes(_ context.Context, f remotes.ListFilter) ([]remotes.Remote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []remotes.Remote{}
	for _, r := range s.remotes {
		if !r.Deleted() && (f.OwnerID.IsZero() || r.OwnerID == f.OwnerID) && (f.Type == "" || r.Type == f.Type) {
			out = append(out, cloneRemote(r))
		}
	}
	slices.SortFunc(out, compareRemotes)
	return out, nil
}

// UpdateRemote implements remotes.Repository.
func (s *Store) UpdateRemote(_ context.Context, r remotes.Remote) (remotes.Remote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.remotes[r.ID]
	if !ok || cur.Deleted() {
		return remotes.Remote{}, fmt.Errorf("remote %s: %w", r.ID, domain.ErrNotFound)
	}
	if cur.Version != r.Version {
		return remotes.Remote{}, fmt.Errorf("remote %s is at version %d, not %d: %w", r.ID, cur.Version, r.Version, domain.ErrConflict)
	}
	next := cur
	next.Name, next.Options, next.SecretsEnc, next.SecretKeys = r.Name, r.Options, r.SecretsEnc, r.SecretKeys
	next.Members, next.Wrapped, next.UpdatedAt = r.Members, r.Wrapped, r.UpdatedAt
	next.Version = cur.Version + 1
	if err := s.checkRemoteLocked(next); err != nil {
		return remotes.Remote{}, fmt.Errorf("update remote %s: %w", r.ID, err)
	}
	next = cloneRemote(next)
	s.remotes[r.ID] = next
	return cloneRemote(next), nil
}

// DeleteRemote implements remotes.Repository.
func (s *Store) DeleteRemote(_ context.Context, id domain.RemoteID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.remotes[id]
	if !ok || r.Deleted() {
		return fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
	}
	if using := s.remotesUsingLocked(id); len(using) > 0 {
		return fmt.Errorf("remote %s: %w: %s", id, domain.ErrFailedPrecondition, remotes.InUseMessage(id, using[0]))
	}
	if name, ok := s.repositoryOnRemoteLocked(id); ok {
		return fmt.Errorf("%w: the remote holds repository %q; delete the repository first", domain.ErrFailedPrecondition, name)
	}
	r.DeletedAt, r.SecretsEnc = at, nil
	r.Version++
	s.remotes[id] = r
	return nil
}

// ListRemotesUsing implements remotes.Repository.
func (s *Store) ListRemotesUsing(_ context.Context, id domain.RemoteID) ([]remotes.Remote, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.remotesUsingLocked(id)
	for i := range out {
		out[i] = cloneRemote(out[i])
	}
	return out, nil
}

// remotesUsingLocked returns the live remotes that have id among their
// members or wrap it, sorted; the values alias the store.
func (s *Store) remotesUsingLocked(id domain.RemoteID) []remotes.Remote {
	out := []remotes.Remote{}
	for _, r := range s.remotes {
		if !r.Deleted() && (r.Wrapped.RemoteID == id || slices.ContainsFunc(r.Members, func(m remotes.Member) bool { return m.RemoteID == id })) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, compareRemotes)
	return out
}
