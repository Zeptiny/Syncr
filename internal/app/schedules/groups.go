package schedules

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Groups organize a user's schedule list (ADR 0024): a schedule is in at
// most one of its owner's groups, and nothing that runs schedules reads
// them. Like schedules, groups are private to their owner, admins
// included: another user's group is NotFound.

// ErrUnknownGroup reports a group that is not one of the actor's (Create,
// Update, MoveSchedules and the group use cases). It matches
// domain.ErrNotFound, so another user's group looks like a missing one.
var ErrUnknownGroup = fmt.Errorf("group %w", domain.ErrNotFound)

// CreateGroup creates a group of actor. Errors are a
// *domain.ValidationError ("name") or ErrAlreadyExists for a name the actor
// already uses, case-insensitively.
func (s *Service) CreateGroup(ctx context.Context, actor domain.Actor, name string) (Group, error) {
	if actor.UserID.IsZero() {
		return Group{}, fmt.Errorf("create schedule group: %w: groups belong to a user account", domain.ErrPermissionDenied)
	}
	var v domain.Validation
	name = checkDisplayName(&v, "name", name, MaxGroupNameLen)
	if err := v.Err(); err != nil {
		return Group{}, fmt.Errorf("create schedule group: %w", err)
	}
	now := s.clock.Now()
	g := Group{ID: s.ids.NewScheduleGroupID(), OwnerID: actor.UserID, Name: name, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.CreateGroup(ctx, g); err != nil {
		return Group{}, fmt.Errorf("create schedule group: %w", groupNameTaken(err, name))
	}
	s.log.InfoContext(ctx, "schedule group created", "group_id", g.ID.String(), "owner_id", actor.UserID.String())
	return g, nil
}

// Groups returns actor's groups, by name.
func (s *Service) Groups(ctx context.Context, actor domain.Actor) ([]Group, error) {
	if actor.UserID.IsZero() {
		return []Group{}, nil
	}
	out, err := s.repo.ListGroups(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("list schedule groups: %w", err)
	}
	return out, nil
}

// RenameGroup renames one of actor's groups (errors as CreateGroup's, and
// ErrUnknownGroup).
func (s *Service) RenameGroup(ctx context.Context, actor domain.Actor, id domain.ScheduleGroupID, name string) (Group, error) {
	if _, err := s.ownedGroup(ctx, actor, id); err != nil {
		return Group{}, fmt.Errorf("rename schedule group: %w", err)
	}
	var v domain.Validation
	name = checkDisplayName(&v, "name", name, MaxGroupNameLen)
	if err := v.Err(); err != nil {
		return Group{}, fmt.Errorf("rename schedule group %s: %w", id, err)
	}
	g, err := s.repo.RenameGroup(ctx, id, name, s.clock.Now())
	if err != nil {
		return Group{}, fmt.Errorf("rename schedule group %s: %w", id, groupNameTaken(err, name))
	}
	s.log.InfoContext(ctx, "schedule group renamed", "group_id", id.String())
	return g, nil
}

// DeleteGroup deletes one of actor's groups. Its schedules stay, in no
// group; they keep running as before.
func (s *Service) DeleteGroup(ctx context.Context, actor domain.Actor, id domain.ScheduleGroupID) error {
	if _, err := s.ownedGroup(ctx, actor, id); err != nil {
		return fmt.Errorf("delete schedule group: %w", err)
	}
	if err := s.repo.DeleteGroup(ctx, id, s.clock.Now()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("delete schedule group %s: %w: %w", id, ErrUnknownGroup, err)
		}
		return fmt.Errorf("delete schedule group %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "schedule group deleted", "group_id", id.String())
	return nil
}

// MoveSchedules moves actor's schedules ids into the group (zero: out of
// any group), all or none, without a version check (like SetEnabled), and
// returns them by ID. Every schedule must be the actor's (ErrNotFound
// otherwise) and the group one of the actor's groups (ErrUnknownGroup).
// The ids are 1 to MaxMove valid IDs (a *domain.ValidationError
// "schedule_ids" otherwise); duplicates count once.
func (s *Service) MoveSchedules(ctx context.Context, actor domain.Actor, ids []domain.ScheduleID, group domain.ScheduleGroupID) ([]Schedule, error) {
	var v domain.Validation
	switch {
	case len(ids) == 0:
		v.Add("schedule_ids", "is required")
	case len(ids) > MaxMove:
		v.Add("schedule_ids", fmt.Sprintf("at most %d schedules can be moved at once", MaxMove))
	case slices.ContainsFunc(ids, func(id domain.ScheduleID) bool { return id.IsZero() }):
		v.Add("schedule_ids", "has an invalid ID")
	}
	if err := v.Err(); err != nil {
		return nil, fmt.Errorf("move schedules: %w", err)
	}
	ids = sortedIDs(ids)
	if !group.IsZero() {
		if _, err := s.ownedGroup(ctx, actor, group); err != nil {
			return nil, fmt.Errorf("move schedules: %w", err)
		}
	}
	scs, err := s.repo.GetSchedules(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("move schedules: %w", err)
	}
	if len(scs) != len(ids) {
		return nil, fmt.Errorf("move schedules: schedule %w", domain.ErrNotFound)
	}
	for _, sc := range scs {
		if !actor.Owns(sc.OwnerID) {
			return nil, fmt.Errorf("move schedules: schedule %s: %w", sc.ID, errForeign)
		}
	}
	out, err := s.repo.MoveSchedules(ctx, ids, group, s.clock.Now())
	if err != nil {
		return nil, fmt.Errorf("move schedules: %w", err)
	}
	s.log.InfoContext(ctx, "schedules moved", "count", len(out), "group_id", groupLog(group))
	return out, nil
}

// ownedGroup returns group id of actor, or ErrUnknownGroup.
func (s *Service) ownedGroup(ctx context.Context, actor domain.Actor, id domain.ScheduleGroupID) (Group, error) {
	if id.IsZero() {
		return Group{}, fmt.Errorf("schedule group: %w", domain.ErrInvalidID)
	}
	return s.groupOf(ctx, actor.UserID, id)
}

// groupOf returns group id if owner owns it, or ErrUnknownGroup.
func (s *Service) groupOf(ctx context.Context, owner domain.UserID, id domain.ScheduleGroupID) (Group, error) {
	g, err := s.repo.GetGroup(ctx, id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return Group{}, fmt.Errorf("%w: %s", ErrUnknownGroup, id)
	case err != nil:
		return Group{}, fmt.Errorf("schedule group %s: %w", id, err)
	case owner.IsZero() || g.OwnerID != owner:
		return Group{}, fmt.Errorf("%w: %s is another user's", ErrUnknownGroup, id)
	}
	return g, nil
}

// groupNameTaken explains a repository ErrAlreadyExists of a group (its
// owner's names are the only unique key a valid group can hit).
func groupNameTaken(err error, name string) error {
	if errors.Is(err, domain.ErrAlreadyExists) {
		return fmt.Errorf("you already have a group named %q: %w", name, err)
	}
	return err
}

func groupLog(id domain.ScheduleGroupID) string {
	if id.IsZero() {
		return ""
	}
	return id.String()
}
