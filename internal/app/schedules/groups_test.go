package schedules_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestGroupsAreTheOwners(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	photos, err := e.svc.CreateGroup(ctx, e.alice, "  Photos ")
	if err != nil || photos.Name != "Photos" || photos.OwnerID != e.alice.UserID || !photos.CreatedAt.Equal(t0) {
		t.Fatalf("CreateGroup = (%+v, %v), want Photos, trimmed, of alice", photos, err)
	}
	if _, err := e.svc.CreateGroup(ctx, e.alice, "photos"); !errors.Is(err, domain.ErrAlreadyExists) || !strings.Contains(err.Error(), "you already have a group named") {
		t.Errorf("CreateGroup(taken) = %v", err)
	}
	var verr *domain.ValidationError
	if _, err := e.svc.CreateGroup(ctx, e.alice, " "); !errors.As(err, &verr) || verr.Fields[0].Field != "name" {
		t.Errorf("CreateGroup(blank) = %v, want a name error", err)
	}
	if _, err := e.svc.CreateGroup(ctx, e.alice, strings.Repeat("é", schedules.MaxGroupNameLen+1)); !errors.As(err, &verr) {
		t.Errorf("CreateGroup(too long) = %v", err)
	}
	if _, err := e.svc.CreateGroup(ctx, domain.Actor{}, "Photos"); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("CreateGroup(no user) = %v", err)
	}

	// Another user, admins included, sees none of them and changes none.
	for _, a := range []domain.Actor{e.bob, e.admin} {
		if gs, err := e.svc.Groups(ctx, a); err != nil || len(gs) != 0 {
			t.Errorf("Groups(%v) = (%v, %v), want none", a.Role, gs, err)
		}
		if _, err := e.svc.RenameGroup(ctx, a, photos.ID, "Mine"); !errors.Is(err, schedules.ErrUnknownGroup) {
			t.Errorf("RenameGroup(another's) = %v", err)
		}
		if err := e.svc.DeleteGroup(ctx, a, photos.ID); !errors.Is(err, schedules.ErrUnknownGroup) {
			t.Errorf("DeleteGroup(another's) = %v", err)
		}
		in := e.input("Theirs")
		in.GroupID = photos.ID
		if _, err := e.svc.Create(ctx, a, in); !errors.Is(err, schedules.ErrUnknownGroup) {
			t.Errorf("Create in another's group = %v", err)
		}
	}

	e.clock.Advance(time.Minute)
	renamed, err := e.svc.RenameGroup(ctx, e.alice, photos.ID, "Pictures")
	if err != nil || renamed.Name != "Pictures" || !renamed.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("RenameGroup = (%+v, %v)", renamed, err)
	}
	if gs, err := e.svc.Groups(ctx, e.alice); err != nil || len(gs) != 1 || gs[0].Name != "Pictures" {
		t.Errorf("Groups(alice) = (%+v, %v)", gs, err)
	}
}

func TestSchedulesInGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	photos, _ := e.svc.CreateGroup(ctx, e.alice, "Photos")
	office, _ := e.svc.CreateGroup(ctx, e.alice, "Office")
	theirs, _ := e.svc.CreateGroup(ctx, e.bob, "Theirs")

	in := e.input("Nightly")
	in.GroupID = photos.ID
	a := e.create(e.alice, in)
	if a.GroupID != photos.ID {
		t.Fatalf("Create in a group: GroupID = %v", a.GroupID)
	}
	b := e.create(e.alice, e.input("Hourly"))
	in.GroupID = theirs.ID
	if _, err := e.svc.Update(ctx, e.alice, a.ID, a.Version, in); !errors.Is(err, schedules.ErrUnknownGroup) {
		t.Errorf("Update into another's group = %v", err)
	}
	wakes := e.waker.n.Load()

	moved, err := e.svc.MoveSchedules(ctx, e.alice, []domain.ScheduleID{b.ID, a.ID, b.ID}, office.ID)
	if err != nil || len(moved) != 2 {
		t.Fatalf("MoveSchedules = (%d, %v)", len(moved), err)
	}
	for _, sc := range moved {
		if sc.GroupID != office.ID || sc.Version != 2 {
			t.Errorf("moved %s: group %v v%d", sc.Name, sc.GroupID, sc.Version)
		}
	}
	if e.waker.n.Load() != wakes {
		t.Error("a move woke the scheduler: groups change nothing it reads")
	}
	if _, err := e.svc.Update(ctx, e.alice, a.ID, a.Version, e.input("Nightly")); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("an edit read before the move = %v, want a version conflict", err)
	}

	bobs := e.create(e.bob, e.input("Bob's"))
	for name, ids := range map[string][]domain.ScheduleID{
		"another's schedule": {a.ID, bobs.ID},
		"a missing schedule": {a.ID, domain.NewScheduleID()},
	} {
		if _, err := e.svc.MoveSchedules(ctx, e.alice, ids, photos.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("MoveSchedules(%s) = %v", name, err)
		}
	}
	if _, err := e.svc.MoveSchedules(ctx, e.alice, []domain.ScheduleID{a.ID}, theirs.ID); !errors.Is(err, schedules.ErrUnknownGroup) {
		t.Errorf("MoveSchedules(another's group) = %v", err)
	}
	if got, _ := e.svc.Get(ctx, e.alice, a.ID); got.GroupID != office.ID {
		t.Errorf("after refused moves: group %v, want Office", got.GroupID)
	}
	var verr *domain.ValidationError
	if _, err := e.svc.MoveSchedules(ctx, e.alice, nil, photos.ID); !errors.As(err, &verr) {
		t.Errorf("MoveSchedules(none) = %v", err)
	}
	if _, err := e.svc.MoveSchedules(ctx, e.alice, make([]domain.ScheduleID, schedules.MaxMove+1), photos.ID); !errors.As(err, &verr) {
		t.Errorf("MoveSchedules(too many) = %v", err)
	}

	// Health names the groups (the metrics' group label).
	health, err := e.svc.Health(ctx, e.alice)
	if err != nil || len(health) != 2 {
		t.Fatalf("Health = (%d, %v)", len(health), err)
	}
	for _, h := range health {
		if h.GroupName != "Office" {
			t.Errorf("Health(%s).GroupName = %q, want Office", h.Schedule.Name, h.GroupName)
		}
	}

	// Deleting a group keeps its schedules, running, in no group.
	if err := e.svc.DeleteGroup(ctx, e.alice, office.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Get(ctx, e.alice, a.ID)
	if err != nil || !got.GroupID.IsZero() || !got.Enabled || got.NextRunAt.IsZero() {
		t.Errorf("after deleting its group: %+v, %v", got, err)
	}
	// A schedule paused and resumed after its group went still resumes.
	if _, err := e.svc.SetEnabled(ctx, e.alice, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetEnabled(ctx, e.alice, a.ID, true); err != nil {
		t.Errorf("resume = %v", err)
	}
}
