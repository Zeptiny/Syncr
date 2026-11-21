package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/postgres/db"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Schedule groups live in schedule_groups (migration 00014). A schedule's
// group_id references (id, owner_id), so it can only name a group of the
// schedule's own owner.

// CreateGroup implements schedules.Repository.
func (s *Store) CreateGroup(ctx context.Context, g schedules.Group) error {
	if err := s.q.CreateScheduleGroup(ctx, db.CreateScheduleGroupParams{
		ID: pgID(g.ID), OwnerID: pgID(g.OwnerID), Name: g.Name,
		CreatedAt: pgTime(g.CreatedAt), UpdatedAt: pgTime(g.UpdatedAt),
	}); err != nil {
		return mapErr(fmt.Sprintf("create schedule group %s", g.ID), err)
	}
	return nil
}

// GetGroup implements schedules.Repository.
func (s *Store) GetGroup(ctx context.Context, id domain.ScheduleGroupID) (schedules.Group, error) {
	op := fmt.Sprintf("get schedule group %s", id)
	row, err := s.q.GetScheduleGroup(ctx, pgID(id))
	if err != nil {
		return schedules.Group{}, mapErr(op, err)
	}
	g, err := groupFrom(row)
	if err != nil {
		return schedules.Group{}, fmt.Errorf("%s: %w", op, err)
	}
	return g, nil
}

// ListGroups implements schedules.Repository.
func (s *Store) ListGroups(ctx context.Context, owner domain.UserID) ([]schedules.Group, error) {
	const op = "list schedule groups"
	rows, err := s.q.ListScheduleGroups(ctx, pgOptID(owner))
	if err != nil {
		return nil, mapErr(op, err)
	}
	out := make([]schedules.Group, 0, len(rows))
	for _, row := range rows {
		g, err := groupFrom(row)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, g)
	}
	return out, nil
}

// RenameGroup implements schedules.Repository.
func (s *Store) RenameGroup(ctx context.Context, id domain.ScheduleGroupID, name string, at time.Time) (schedules.Group, error) {
	op := fmt.Sprintf("rename schedule group %s", id)
	row, err := s.q.RenameScheduleGroup(ctx, db.RenameScheduleGroupParams{Name: name, UpdatedAt: pgTime(at), ID: pgID(id)})
	if err != nil {
		return schedules.Group{}, mapErr(op, err) // no row: ErrNotFound
	}
	g, err := groupFrom(row)
	if err != nil {
		return schedules.Group{}, fmt.Errorf("%s: %w", op, err)
	}
	return g, nil
}

// DeleteGroup implements schedules.Repository: the group's schedules are
// moved out of it, then the group is deleted, in one transaction.
func (s *Store) DeleteGroup(ctx context.Context, id domain.ScheduleGroupID, at time.Time) error {
	op := fmt.Sprintf("delete schedule group %s", id)
	return s.inTx(ctx, op, func(q *db.Queries) error {
		if err := q.UngroupSchedules(ctx, db.UngroupSchedulesParams{UpdatedAt: pgTime(at), GroupID: pgID(id)}); err != nil {
			return mapErr(op, err)
		}
		n, err := q.DeleteScheduleGroup(ctx, pgID(id))
		if err != nil {
			return mapErr(op, err)
		}
		if n == 0 {
			return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil
	})
}

// MoveSchedules implements schedules.Repository: one update of every
// schedule, then they are read back, in one transaction; a missing
// schedule (fewer rows than IDs) or group (the foreign key) rolls it back.
func (s *Store) MoveSchedules(ctx context.Context, ids []domain.ScheduleID, group domain.ScheduleGroupID, at time.Time) ([]schedules.Schedule, error) {
	const op = "move schedules"
	ids = sortedUniqueIDs(ids)
	var out []schedules.Schedule
	err := s.inTx(ctx, op, func(q *db.Queries) error {
		n, err := q.MoveSchedules(ctx, db.MoveSchedulesParams{GroupID: pgOptID(group), UpdatedAt: pgTime(at), Ids: pgIDs(ids)})
		if err != nil {
			return mapErr(op, err) // the foreign key: ErrNotFound
		}
		if n != int64(len(ids)) {
			return fmt.Errorf("%s: %d of %d schedules exist: %w", op, n, len(ids), domain.ErrNotFound)
		}
		rows, err := q.GetSchedules(ctx, pgIDs(ids))
		if err != nil {
			return mapErr(op, err)
		}
		out, err = withScheduleLinks(ctx, q, op, rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// groupFrom converts a group row.
func groupFrom(row db.ScheduleGroup) (schedules.Group, error) {
	g := schedules.Group{Name: row.Name, CreatedAt: timeFrom(row.CreatedAt), UpdatedAt: timeFrom(row.UpdatedAt)}
	if err := decodeID(&g.ID, row.ID); err != nil {
		return schedules.Group{}, err
	}
	if err := decodeID(&g.OwnerID, row.OwnerID); err != nil {
		return schedules.Group{}, err
	}
	return g, nil
}
