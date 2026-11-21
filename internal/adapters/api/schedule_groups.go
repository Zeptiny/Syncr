package api

import (
	"context"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// The schedule groups of ScheduleService (ADR 0024): the caller's own,
// admins included, like schedules.

// ListScheduleGroups lists the caller's groups, by name.
func (h *scheduleHandler) ListScheduleGroups(ctx context.Context, _ *connect.Request[syncrv1.ListScheduleGroupsRequest]) (*connect.Response[syncrv1.ListScheduleGroupsResponse], error) {
	actor, _ := ActorFrom(ctx)
	gs, err := h.schedules.Groups(ctx, actor)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ScheduleServiceListScheduleGroupsProcedure, err)
	}
	resp := &syncrv1.ListScheduleGroupsResponse{Groups: make([]*syncrv1.ScheduleGroup, 0, len(gs))}
	for _, g := range gs {
		resp.Groups = append(resp.Groups, scheduleGroupToProto(g))
	}
	return connect.NewResponse(resp), nil
}

// CreateScheduleGroup creates a group of the caller.
func (h *scheduleHandler) CreateScheduleGroup(ctx context.Context, req *connect.Request[syncrv1.CreateScheduleGroupRequest]) (*connect.Response[syncrv1.CreateScheduleGroupResponse], error) {
	actor, _ := ActorFrom(ctx)
	g, err := h.schedules.CreateGroup(ctx, actor, req.Msg.GetName())
	if err != nil {
		return nil, h.failGroup(ctx, syncrv1connect.ScheduleServiceCreateScheduleGroupProcedure, err)
	}
	return connect.NewResponse(&syncrv1.CreateScheduleGroupResponse{Group: scheduleGroupToProto(g)}), nil
}

// RenameScheduleGroup renames one of the caller's groups.
func (h *scheduleHandler) RenameScheduleGroup(ctx context.Context, req *connect.Request[syncrv1.RenameScheduleGroupRequest]) (*connect.Response[syncrv1.RenameScheduleGroupResponse], error) {
	id, err := parseScheduleGroupID("group_id", req.Msg.GetGroupId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	g, err := h.schedules.RenameGroup(ctx, actor, id, req.Msg.GetName())
	if err != nil {
		return nil, h.failGroup(ctx, syncrv1connect.ScheduleServiceRenameScheduleGroupProcedure, err)
	}
	return connect.NewResponse(&syncrv1.RenameScheduleGroupResponse{Group: scheduleGroupToProto(g)}), nil
}

// DeleteScheduleGroup deletes one of the caller's groups; its schedules
// stay, in no group.
func (h *scheduleHandler) DeleteScheduleGroup(ctx context.Context, req *connect.Request[syncrv1.DeleteScheduleGroupRequest]) (*connect.Response[syncrv1.DeleteScheduleGroupResponse], error) {
	id, err := parseScheduleGroupID("group_id", req.Msg.GetGroupId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	if err := h.schedules.DeleteGroup(ctx, actor, id); err != nil {
		return nil, h.failGroup(ctx, syncrv1connect.ScheduleServiceDeleteScheduleGroupProcedure, err)
	}
	return connect.NewResponse(&syncrv1.DeleteScheduleGroupResponse{}), nil
}

// MoveSchedules moves the caller's schedules into one of the caller's
// groups, or out of their group.
func (h *scheduleHandler) MoveSchedules(ctx context.Context, req *connect.Request[syncrv1.MoveSchedulesRequest]) (*connect.Response[syncrv1.MoveSchedulesResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceMoveSchedulesProcedure
	raw := req.Msg.GetScheduleIds()
	if len(raw) > schedules.MaxMove {
		// Before parsing: a huge request is refused without the work.
		return nil, invalidField("schedule_ids", "at most 500 schedules can be moved at once")
	}
	ids := make([]domain.ScheduleID, 0, len(raw))
	for _, s := range raw {
		id, err := domain.ParseScheduleID(s)
		if err != nil || id.IsZero() {
			return nil, invalidField("schedule_ids", "has an invalid ID")
		}
		ids = append(ids, id)
	}
	var group domain.ScheduleGroupID
	if s := req.Msg.GetGroupId(); s != "" {
		id, err := parseScheduleGroupID("group_id", s)
		if err != nil {
			return nil, err
		}
		group = id
	}
	actor, _ := ActorFrom(ctx)
	moved, err := h.schedules.MoveSchedules(ctx, actor, ids, group)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msgs, err := h.protoSchedules(ctx, actor, moved)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.MoveSchedulesResponse{Schedules: msgs}), nil
}

// failGroup maps the errors of the group use cases, naming the group.
func (h *scheduleHandler) failGroup(ctx context.Context, procedure string, err error) error {
	return h.errs.toConnect(ctx, procedure, "group", err)
}

// parseScheduleGroupID parses a request's group ID into an
// InvalidArgument error naming field.
func parseScheduleGroupID(field, s string) (domain.ScheduleGroupID, error) {
	if s == "" {
		return domain.ScheduleGroupID{}, invalidField(field, "is required")
	}
	id, err := domain.ParseScheduleGroupID(s)
	if err != nil || id.IsZero() {
		return domain.ScheduleGroupID{}, invalidField(field, "is not a valid ID")
	}
	return id, nil
}

// scheduleGroupToProto converts a group.
func scheduleGroupToProto(g schedules.Group) *syncrv1.ScheduleGroup {
	return &syncrv1.ScheduleGroup{Id: g.ID.String(), Name: g.Name, CreatedAt: ts(g.CreatedAt), UpdatedAt: ts(g.UpdatedAt)}
}
