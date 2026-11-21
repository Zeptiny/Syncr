package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of the schedule groups of
// schedules.Repository.

func groupID(t *testing.T, n int) domain.ScheduleGroupID {
	return id(t, domain.ParseScheduleGroupID, 10000+n)
}

func newGroup(t *testing.T, n int, owner domain.UserID, name string) schedules.Group {
	return schedules.Group{ID: groupID(t, n), OwnerID: owner, Name: name, CreatedAt: t0(), UpdatedAt: t0()}
}

func mustCreateGroup(t *testing.T, s Store, g schedules.Group) {
	t.Helper()
	if err := s.CreateGroup(context.Background(), g); err != nil {
		t.Fatalf("CreateGroup(%s): %v", g.Name, err)
	}
}

func groupIDs(gs []schedules.Group) []domain.ScheduleGroupID {
	out := make([]domain.ScheduleGroupID, len(gs))
	for i, g := range gs {
		out[i] = g.ID
	}
	return out
}

func testScheduleGroups(t *testing.T, s Store) {
	ctx := context.Background()
	alice, bob := userID(t, 1), userID(t, 2)
	requireSchedules(t, s, alice, bob)

	photos := newGroup(t, 1, alice, "Photos")
	nas := newGroup(t, 2, alice, "nas")
	theirs := newGroup(t, 3, bob, "photos") // another owner may use the name
	for _, g := range []schedules.Group{photos, nas, theirs} {
		mustCreateGroup(t, s, g)
	}
	wantErr(t, "CreateGroup(duplicate id)", s.CreateGroup(ctx, photos), domain.ErrAlreadyExists)
	wantErr(t, "CreateGroup(name taken, other case)", s.CreateGroup(ctx, newGroup(t, 4, alice, "PHOTOS")), domain.ErrAlreadyExists)
	wantErr(t, "CreateGroup(unknown owner)", s.CreateGroup(ctx, newGroup(t, 5, userID(t, 99), "Lost")), domain.ErrNotFound)
	wantErr(t, "CreateGroup(empty name)", s.CreateGroup(ctx, newGroup(t, 6, alice, "")), domain.ErrInvalidArgument)
	wantErr(t, "CreateGroup(long name)", s.CreateGroup(ctx, newGroup(t, 7, alice, strings.Repeat("x", schedules.MaxGroupNameLen+1))), domain.ErrInvalidArgument)

	got, err := s.GetGroup(ctx, photos.ID)
	if err != nil || got.OwnerID != alice || got.Name != "Photos" || !got.CreatedAt.Equal(t0()) || !got.UpdatedAt.Equal(t0()) {
		t.Errorf("GetGroup = (%+v, %v)", got, err)
	}
	_, err = s.GetGroup(ctx, groupID(t, 99))
	wantErr(t, "GetGroup(missing)", err, domain.ErrNotFound)

	mine, err := s.ListGroups(ctx, alice)
	if err != nil || !slices.Equal(groupIDs(mine), []domain.ScheduleGroupID{nas.ID, photos.ID}) {
		t.Errorf("ListGroups(alice) = (%v, %v), want by lower-cased name", groupIDs(mine), err)
	}
	all, err := s.ListGroups(ctx, domain.UserID{})
	if err != nil || !slices.Equal(groupIDs(all), []domain.ScheduleGroupID{nas.ID, photos.ID, theirs.ID}) {
		t.Errorf("ListGroups() = (%v, %v), want every owner's by name, then ID", groupIDs(all), err)
	}
	if none, err := s.ListGroups(ctx, userID(t, 98)); err != nil || none == nil || len(none) != 0 {
		t.Errorf("ListGroups(nobody) = (%v, %v), want an empty list", none, err)
	}

	at := t0().Add(time.Minute)
	renamed, err := s.RenameGroup(ctx, nas.ID, "NAS shares", at)
	if err != nil || renamed.Name != "NAS shares" || !renamed.UpdatedAt.Equal(at) || !renamed.CreatedAt.Equal(t0()) || renamed.OwnerID != alice {
		t.Errorf("RenameGroup = (%+v, %v)", renamed, err)
	}
	if again, err := s.RenameGroup(ctx, nas.ID, "nas SHARES", at); err != nil || again.Name != "nas SHARES" {
		t.Errorf("RenameGroup(own name, other case) = (%+v, %v)", again, err)
	}
	_, err = s.RenameGroup(ctx, nas.ID, "photos", at)
	wantErr(t, "RenameGroup(name taken)", err, domain.ErrAlreadyExists)
	_, err = s.RenameGroup(ctx, groupID(t, 99), "Lost", at)
	wantErr(t, "RenameGroup(missing)", err, domain.ErrNotFound)
	if g, _ := s.GetGroup(ctx, nas.ID); g.Name != "nas SHARES" {
		t.Errorf("after a refused rename: %q", g.Name)
	}
}

func testScheduleGroupReferences(t *testing.T, s Store) {
	ctx := context.Background()
	alice, bob := userID(t, 1), userID(t, 2)
	requireSchedules(t, s, alice, bob)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	photos := newGroup(t, 1, alice, "Photos")
	office := newGroup(t, 2, alice, "Office")
	theirs := newGroup(t, 3, bob, "Theirs")
	for _, g := range []schedules.Group{photos, office, theirs} {
		mustCreateGroup(t, s, g)
	}

	a := newSchedule(t, 1, alice, "Nightly", w.ID)
	a.GroupID = photos.ID
	mustCreateSchedule(t, s, a)
	if got := mustGetSchedule(t, s, a.ID); got.GroupID != photos.ID {
		t.Errorf("GetSchedule.GroupID = %v, want %v", got.GroupID, photos.ID)
	}
	plain := newSchedule(t, 2, alice, "Plain", w.ID)
	mustCreateSchedule(t, s, plain)
	if got := mustGetSchedule(t, s, plain.ID); !got.GroupID.IsZero() {
		t.Errorf("a schedule created in no group has group %v", got.GroupID)
	}
	missing := newSchedule(t, 3, alice, "Missing group", w.ID)
	missing.GroupID = groupID(t, 99)
	wantErr(t, "CreateSchedule(missing group)", s.CreateSchedule(ctx, missing), domain.ErrNotFound)
	foreign := newSchedule(t, 4, alice, "Foreign group", w.ID)
	foreign.GroupID = theirs.ID
	wantErr(t, "CreateSchedule(another owner's group)", s.CreateSchedule(ctx, foreign), domain.ErrNotFound)

	next := mustGetSchedule(t, s, a.ID)
	next.GroupID, next.UpdatedAt = office.ID, t0().Add(time.Minute)
	stored, err := s.UpdateSchedule(ctx, next, false)
	if err != nil || stored.GroupID != office.ID || stored.Version != 2 {
		t.Errorf("UpdateSchedule(other group) = (%v v%d, %v)", stored.GroupID, stored.Version, err)
	}
	next = mustGetSchedule(t, s, a.ID)
	next.GroupID = theirs.ID
	_, err = s.UpdateSchedule(ctx, next, false)
	wantErr(t, "UpdateSchedule(another owner's group)", err, domain.ErrNotFound)
	next.GroupID = domain.ScheduleGroupID{}
	if stored, err := s.UpdateSchedule(ctx, next, false); err != nil || !stored.GroupID.IsZero() {
		t.Errorf("UpdateSchedule(no group) = (%v, %v)", stored.GroupID, err)
	}
	list, err := s.ListSchedules(ctx, schedules.ListFilter{OwnerID: alice})
	if err != nil || len(list) != 2 || !list[0].GroupID.IsZero() || !list[1].GroupID.IsZero() {
		t.Errorf("ListSchedules after ungrouping = (%+v, %v)", list, err)
	}
}

func testScheduleMove(t *testing.T, s Store) {
	ctx := context.Background()
	alice, bob := userID(t, 1), userID(t, 2)
	requireSchedules(t, s, alice, bob)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	photos := newGroup(t, 1, alice, "Photos")
	theirs := newGroup(t, 2, bob, "Theirs")
	mustCreateGroup(t, s, photos)
	mustCreateGroup(t, s, theirs)
	a := newSchedule(t, 1, alice, "A", w.ID)
	b := newSchedule(t, 2, alice, "B", w.ID)
	c := newSchedule(t, 3, alice, "C", w.ID)
	for _, sc := range []schedules.Schedule{a, b, c} {
		mustCreateSchedule(t, s, sc)
	}

	at := t0().Add(time.Minute)
	moved, err := s.MoveSchedules(ctx, []domain.ScheduleID{b.ID, a.ID, b.ID}, photos.ID, at)
	if err != nil || !slices.Equal(scheduleIDs(moved), []domain.ScheduleID{a.ID, b.ID}) {
		t.Fatalf("MoveSchedules = (%v, %v), want a and b by ID", scheduleIDs(moved), err)
	}
	for _, sc := range moved {
		if sc.GroupID != photos.ID || sc.Version != 2 || !sc.UpdatedAt.Equal(at) || sc.Cron != a.Cron || !sameJSON(sc.Definition, a.Definition) {
			t.Errorf("moved %s = %+v, want in Photos at version 2, the rest unchanged", sc.Name, sc)
		}
	}
	if got := mustGetSchedule(t, s, c.ID); !got.GroupID.IsZero() || got.Version != 1 {
		t.Errorf("a schedule not moved = (%v v%d)", got.GroupID, got.Version)
	}

	// All or nothing.
	_, err = s.MoveSchedules(ctx, []domain.ScheduleID{c.ID, scheduleID(t, 99)}, photos.ID, at)
	wantErr(t, "MoveSchedules(a missing schedule)", err, domain.ErrNotFound)
	_, err = s.MoveSchedules(ctx, []domain.ScheduleID{c.ID}, groupID(t, 99), at)
	wantErr(t, "MoveSchedules(missing group)", err, domain.ErrNotFound)
	_, err = s.MoveSchedules(ctx, []domain.ScheduleID{c.ID, a.ID}, theirs.ID, at)
	wantErr(t, "MoveSchedules(another owner's group)", err, domain.ErrNotFound)
	if got := mustGetSchedule(t, s, c.ID); !got.GroupID.IsZero() || got.Version != 1 {
		t.Errorf("after refused moves, c = (%v v%d), want unchanged", got.GroupID, got.Version)
	}
	if got := mustGetSchedule(t, s, a.ID); got.GroupID != photos.ID || got.Version != 2 {
		t.Errorf("after refused moves, a = (%v v%d), want unchanged", got.GroupID, got.Version)
	}

	// Out of every group; a stale edit then fails its version check.
	out, err := s.MoveSchedules(ctx, []domain.ScheduleID{a.ID}, domain.ScheduleGroupID{}, at.Add(time.Minute))
	if err != nil || len(out) != 1 || !out[0].GroupID.IsZero() || out[0].Version != 3 {
		t.Errorf("MoveSchedules(no group) = (%+v, %v)", out, err)
	}
	stale := a
	stale.Version = 2
	_, err = s.UpdateSchedule(ctx, stale, false)
	wantErr(t, "UpdateSchedule after a move", err, domain.ErrConflict)
}

func testScheduleGroupDelete(t *testing.T, s Store) {
	ctx := context.Background()
	alice := userID(t, 1)
	requireSchedules(t, s, alice)
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	photos := newGroup(t, 1, alice, "Photos")
	office := newGroup(t, 2, alice, "Office")
	mustCreateGroup(t, s, photos)
	mustCreateGroup(t, s, office)
	a := newSchedule(t, 1, alice, "A", w.ID)
	a.GroupID = photos.ID
	b := newSchedule(t, 2, alice, "B", w.ID)
	b.GroupID = office.ID
	mustCreateSchedule(t, s, a)
	mustCreateSchedule(t, s, b)

	at := t0().Add(time.Hour)
	if err := s.DeleteGroup(ctx, photos.ID, at); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetGroup(ctx, photos.ID)
	wantErr(t, "GetGroup(deleted)", err, domain.ErrNotFound)
	if got := mustGetSchedule(t, s, a.ID); !got.GroupID.IsZero() || got.Version != 2 || !got.UpdatedAt.Equal(at) || !got.Enabled {
		t.Errorf("the deleted group's schedule = (%v v%d at %v, enabled %v), want kept in no group at version 2", got.GroupID, got.Version, got.UpdatedAt, got.Enabled)
	}
	if got := mustGetSchedule(t, s, b.ID); got.GroupID != office.ID || got.Version != 1 {
		t.Errorf("another group's schedule = (%v v%d), want unchanged", got.GroupID, got.Version)
	}
	wantErr(t, "DeleteGroup(deleted)", s.DeleteGroup(ctx, photos.ID, at), domain.ErrNotFound)

	// A user's groups go with the user.
	u := newUser(t, 10)
	mustCreateUser(t, s, u)
	mustCreateGroup(t, s, newGroup(t, 3, u.ID, "Mine"))
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser(owns an empty group) = %v", err)
	}
	if _, err := s.GetGroup(ctx, groupID(t, 3)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the deleted user's group = %v, want gone", err)
	}
}
