package api

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func TestScheduleGroupsAPI(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	ctx := t.Context()
	c := e.schedulesFor(testUser)
	other := e.schedulesFor(testOther)

	mk := func(name string) *syncrv1.ScheduleGroup {
		t.Helper()
		resp, err := c.CreateScheduleGroup(ctx, connect.NewRequest(&syncrv1.CreateScheduleGroupRequest{Name: name}))
		if err != nil {
			t.Fatalf("CreateScheduleGroup(%s): %v", name, err)
		}
		return resp.Msg.GetGroup()
	}
	photos := mk("Photos")
	office := mk("office")
	if photos.GetId() == "" || photos.GetName() != "Photos" || photos.GetCreatedAt() == nil {
		t.Errorf("CreateScheduleGroup = %v", photos)
	}
	_, err := c.CreateScheduleGroup(ctx, connect.NewRequest(&syncrv1.CreateScheduleGroupRequest{Name: "PHOTOS"}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = c.CreateScheduleGroup(ctx, connect.NewRequest(&syncrv1.CreateScheduleGroupRequest{Name: ""}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := violations(t, err); len(v) != 1 || v[0].GetField() != "name" {
		t.Errorf("CreateScheduleGroup(\"\") violations = %v", v)
	}
	// Another user may use the name, and sees only their own groups.
	if _, err := other.CreateScheduleGroup(ctx, connect.NewRequest(&syncrv1.CreateScheduleGroupRequest{Name: "Photos"})); err != nil {
		t.Errorf("CreateScheduleGroup(other user, same name) = %v", err)
	}
	list, err := c.ListScheduleGroups(ctx, connect.NewRequest(&syncrv1.ListScheduleGroupsRequest{}))
	if err != nil || len(list.Msg.GetGroups()) != 2 || list.Msg.GetGroups()[0].GetName() != "office" || list.Msg.GetGroups()[1].GetName() != "Photos" {
		t.Errorf("ListScheduleGroups = (%v, %v), want office and Photos, by name", list.Msg.GetGroups(), err)
	}

	// A schedule in a group.
	in := e.scheduleInput("Nightly")
	in.GroupId = photos.GetId()
	nightly := e.createSchedule(testUser, in)
	if nightly.GetGroupId() != photos.GetId() {
		t.Errorf("CreateSchedule in a group: group_id = %q", nightly.GetGroupId())
	}
	hourly := e.createSchedule(testUser, e.scheduleInput("Hourly"))
	if hourly.GetGroupId() != "" {
		t.Errorf("CreateSchedule without a group: group_id = %q", hourly.GetGroupId())
	}
	bad := e.scheduleInput("Bad")
	bad.GroupId = "not-an-id"
	_, err = c.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: bad}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := violations(t, err); !slices.ContainsFunc(v, func(f *syncrv1.FieldViolation) bool { return f.GetField() == "group_id" }) {
		t.Errorf("CreateSchedule(bad group_id) violations = %v", v)
	}
	theirs := e.scheduleInput("Theirs")
	theirs.GroupId = photos.GetId()
	_, err = other.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: theirs}))
	wantCode(t, err, connect.CodeNotFound)

	// Move both into office, then out of it.
	moved, err := c.MoveSchedules(ctx, connect.NewRequest(&syncrv1.MoveSchedulesRequest{
		ScheduleIds: []string{hourly.GetId(), nightly.GetId()}, GroupId: office.GetId(),
	}))
	if err != nil || len(moved.Msg.GetSchedules()) != 2 {
		t.Fatalf("MoveSchedules = (%v, %v)", moved, err)
	}
	for _, sc := range moved.Msg.GetSchedules() {
		if sc.GetGroupId() != office.GetId() || sc.GetVersion() != 2 || sc.GetDefinition() == nil {
			t.Errorf("moved %s: group %q v%d", sc.GetName(), sc.GetGroupId(), sc.GetVersion())
		}
	}
	_, err = other.MoveSchedules(ctx, connect.NewRequest(&syncrv1.MoveSchedulesRequest{ScheduleIds: []string{hourly.GetId()}}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.MoveSchedules(ctx, connect.NewRequest(&syncrv1.MoveSchedulesRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.MoveSchedules(ctx, connect.NewRequest(&syncrv1.MoveSchedulesRequest{ScheduleIds: []string{"x"}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.MoveSchedules(ctx, connect.NewRequest(&syncrv1.MoveSchedulesRequest{ScheduleIds: []string{hourly.GetId()}, GroupId: "x"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	out, err := c.MoveSchedules(ctx, connect.NewRequest(&syncrv1.MoveSchedulesRequest{ScheduleIds: []string{hourly.GetId()}}))
	if err != nil || out.Msg.GetSchedules()[0].GetGroupId() != "" {
		t.Errorf("MoveSchedules(out of its group) = (%v, %v)", out, err)
	}

	// Rename and delete; another user can do neither.
	_, err = other.RenameScheduleGroup(ctx, connect.NewRequest(&syncrv1.RenameScheduleGroupRequest{GroupId: office.GetId(), Name: "Mine"}))
	wantCode(t, err, connect.CodeNotFound)
	renamed, err := c.RenameScheduleGroup(ctx, connect.NewRequest(&syncrv1.RenameScheduleGroupRequest{GroupId: office.GetId(), Name: "Office PC"}))
	if err != nil || renamed.Msg.GetGroup().GetName() != "Office PC" {
		t.Errorf("RenameScheduleGroup = (%v, %v)", renamed, err)
	}
	_, err = c.RenameScheduleGroup(ctx, connect.NewRequest(&syncrv1.RenameScheduleGroupRequest{GroupId: office.GetId(), Name: "photos"}))
	wantCode(t, err, connect.CodeAlreadyExists)
	_, err = other.DeleteScheduleGroup(ctx, connect.NewRequest(&syncrv1.DeleteScheduleGroupRequest{GroupId: office.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	if _, err := c.DeleteScheduleGroup(ctx, connect.NewRequest(&syncrv1.DeleteScheduleGroupRequest{GroupId: office.GetId()})); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: nightly.GetId()}))
	if err != nil || got.Msg.GetSchedule().GetGroupId() != "" || got.Msg.GetSchedule().GetVersion() != 3 {
		t.Errorf("after deleting its group: %v, %v; want no group at version 3", got.Msg.GetSchedule(), err)
	}
	_, err = c.DeleteScheduleGroup(ctx, connect.NewRequest(&syncrv1.DeleteScheduleGroupRequest{GroupId: office.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.DeleteScheduleGroup(ctx, connect.NewRequest(&syncrv1.DeleteScheduleGroupRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
}
