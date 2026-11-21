package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// The schedule groups of schedules.Repository, with the semantics of
// migration 00014: the case-insensitive name index per owner, the owner's
// foreign key (ON DELETE CASCADE, DeleteUser) and schedules.group_id, which
// must name a group of the schedule's owner (the foreign key (group_id,
// owner_id)).

// groupNameFreeLocked returns ErrAlreadyExists if another group of owner
// has name, case-insensitively.
func (s *Store) groupNameFreeLocked(id domain.ScheduleGroupID, owner domain.UserID, name string) error {
	for _, o := range s.groups {
		if o.ID != id && o.OwnerID == owner && strings.EqualFold(o.Name, name) {
			return fmt.Errorf("group name %q is taken: %w", name, domain.ErrAlreadyExists)
		}
	}
	return nil
}

func checkGroupName(id domain.ScheduleGroupID, name string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > schedules.MaxGroupNameLen {
		return fmt.Errorf("group %s: %w: name length", id, domain.ErrInvalidArgument)
	}
	return nil
}

// CreateGroup implements schedules.Repository.
func (s *Store) CreateGroup(_ context.Context, g schedules.Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[g.ID]; ok {
		return fmt.Errorf("group %s: %w", g.ID, domain.ErrAlreadyExists)
	}
	if _, ok := s.users[g.OwnerID]; !ok {
		return fmt.Errorf("owner %s of group %s: %w", g.OwnerID, g.ID, domain.ErrNotFound)
	}
	if err := checkGroupName(g.ID, g.Name); err != nil {
		return err
	}
	if err := s.groupNameFreeLocked(g.ID, g.OwnerID, g.Name); err != nil {
		return err
	}
	s.groups[g.ID] = g
	return nil
}

// GetGroup implements schedules.Repository.
func (s *Store) GetGroup(_ context.Context, id domain.ScheduleGroupID) (schedules.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return schedules.Group{}, fmt.Errorf("group %s: %w", id, domain.ErrNotFound)
	}
	return g, nil
}

// ListGroups implements schedules.Repository.
func (s *Store) ListGroups(_ context.Context, owner domain.UserID) ([]schedules.Group, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []schedules.Group{}
	for _, g := range s.groups {
		if owner.IsZero() || g.OwnerID == owner {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b schedules.Group) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), compareIDs(a.ID, b.ID))
	})
	return out, nil
}

// RenameGroup implements schedules.Repository.
func (s *Store) RenameGroup(_ context.Context, id domain.ScheduleGroupID, name string, at time.Time) (schedules.Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return schedules.Group{}, fmt.Errorf("group %s: %w", id, domain.ErrNotFound)
	}
	if err := checkGroupName(id, name); err != nil {
		return schedules.Group{}, err
	}
	if err := s.groupNameFreeLocked(id, g.OwnerID, name); err != nil {
		return schedules.Group{}, err
	}
	g.Name, g.UpdatedAt = name, at
	s.groups[id] = g
	return g, nil
}

// DeleteGroup implements schedules.Repository.
func (s *Store) DeleteGroup(_ context.Context, id domain.ScheduleGroupID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[id]; !ok {
		return fmt.Errorf("group %s: %w", id, domain.ErrNotFound)
	}
	for sid, sc := range s.schedules {
		if sc.GroupID == id {
			sc.GroupID, sc.UpdatedAt = domain.ScheduleGroupID{}, at
			sc.Version++
			s.schedules[sid] = sc
		}
	}
	delete(s.groups, id)
	return nil
}

// MoveSchedules implements schedules.Repository.
func (s *Store) MoveSchedules(_ context.Context, ids []domain.ScheduleID, group domain.ScheduleGroupID, at time.Time) ([]schedules.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids = slices.Clone(ids)
	slices.SortFunc(ids, compareIDs)
	ids = slices.Compact(ids)
	g, hasGroup := s.groups[group]
	if !group.IsZero() && !hasGroup {
		return nil, fmt.Errorf("group %s: %w", group, domain.ErrNotFound)
	}
	// Check every schedule before changing any.
	for _, id := range ids {
		sc, ok := s.schedules[id]
		switch {
		case !ok:
			return nil, fmt.Errorf("schedule %s: %w", id, domain.ErrNotFound)
		case !group.IsZero() && sc.OwnerID != g.OwnerID:
			return nil, fmt.Errorf("group %s of schedule %s: %w", group, id, domain.ErrNotFound)
		}
	}
	out := make([]schedules.Schedule, 0, len(ids))
	for _, id := range ids {
		sc := cloneSchedule(s.schedules[id])
		sc.GroupID, sc.UpdatedAt = group, at
		sc.Version++
		s.schedules[id] = sc
		out = append(out, cloneSchedule(sc))
	}
	return out, nil
}
