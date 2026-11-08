package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// scheduleHandler implements syncr.v1.ScheduleService over the schedules
// use cases (Deps.Schedules), checking job definitions with Deps.Kinds as
// RunJob does, and naming the schedules' remotes (Deps.Remotes) and
// contacts (Deps.Contacts). Without Kinds, CreateSchedule and
// UpdateSchedule are Unimplemented; without Remotes or Contacts the
// references have no names.
type scheduleHandler struct {
	syncrv1connect.UnimplementedScheduleServiceHandler

	schedules ScheduleService
	kinds     *jobkind.Registry
	remotes   RemoteService
	contacts  ContactService
	errs      errorMapper
}

var _ syncrv1connect.ScheduleServiceHandler = (*scheduleHandler)(nil)

func (h *scheduleHandler) fail(ctx context.Context, procedure string, err error) error {
	switch {
	case errors.Is(err, schedules.ErrUnknownRemote):
		return connect.NewError(connect.CodeNotFound, errors.New("remote not found"))
	case errors.Is(err, schedules.ErrUnknownContact):
		return connect.NewError(connect.CodeNotFound, errors.New("contact not found"))
	}
	return h.errs.toConnect(ctx, procedure, "schedule", err)
}

// ListSchedules lists the caller's schedules, by name.
func (h *scheduleHandler) ListSchedules(ctx context.Context, req *connect.Request[syncrv1.ListSchedulesRequest]) (*connect.Response[syncrv1.ListSchedulesResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceListSchedulesProcedure
	var f schedules.ListFilter
	if s := req.Msg.GetRemoteId(); s != "" {
		id, err := domain.ParseRemoteID(s)
		if err != nil || id.IsZero() {
			return nil, invalidField("remote_id", "is not a valid ID")
		}
		f.RemoteID = id
	}
	if s := req.Msg.GetContactId(); s != "" {
		id, err := domain.ParseContactID(s)
		if err != nil || id.IsZero() {
			return nil, invalidField("contact_id", "is not a valid ID")
		}
		f.ContactID = id
	}
	actor, _ := ActorFrom(ctx)
	list, err := h.schedules.List(ctx, actor, f)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msgs, err := h.protoSchedules(ctx, actor, list)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.ListSchedulesResponse{Schedules: msgs}), nil
}

// GetSchedule returns one of the caller's schedules.
func (h *scheduleHandler) GetSchedule(ctx context.Context, req *connect.Request[syncrv1.GetScheduleRequest]) (*connect.Response[syncrv1.GetScheduleResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceGetScheduleProcedure
	id, err := parseScheduleID(req.Msg.GetScheduleId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	sc, err := h.schedules.Get(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoSchedule(ctx, actor, sc)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.GetScheduleResponse{Schedule: msg}), nil
}

// CreateSchedule creates a schedule of the caller.
func (h *scheduleHandler) CreateSchedule(ctx context.Context, req *connect.Request[syncrv1.CreateScheduleRequest]) (*connect.Response[syncrv1.CreateScheduleResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceCreateScheduleProcedure
	in, err := h.input(ctx, procedure, req.Msg.GetInput())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	sc, err := h.schedules.Create(ctx, actor, in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoSchedule(ctx, actor, sc)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.CreateScheduleResponse{Schedule: msg}), nil
}

// UpdateSchedule changes one of the caller's schedules (version-checked).
func (h *scheduleHandler) UpdateSchedule(ctx context.Context, req *connect.Request[syncrv1.UpdateScheduleRequest]) (*connect.Response[syncrv1.UpdateScheduleResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceUpdateScheduleProcedure
	id, err := parseScheduleID(req.Msg.GetScheduleId())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetVersion() < 1 {
		return nil, invalidField("version", "is required")
	}
	in, err := h.input(ctx, procedure, req.Msg.GetInput())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	sc, err := h.schedules.Update(ctx, actor, id, req.Msg.GetVersion(), in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoSchedule(ctx, actor, sc)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.UpdateScheduleResponse{Schedule: msg}), nil
}

// SetScheduleEnabled pauses or resumes one of the caller's schedules.
func (h *scheduleHandler) SetScheduleEnabled(ctx context.Context, req *connect.Request[syncrv1.SetScheduleEnabledRequest]) (*connect.Response[syncrv1.SetScheduleEnabledResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceSetScheduleEnabledProcedure
	id, err := parseScheduleID(req.Msg.GetScheduleId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	sc, err := h.schedules.SetEnabled(ctx, actor, id, req.Msg.GetEnabled())
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoSchedule(ctx, actor, sc)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.SetScheduleEnabledResponse{Schedule: msg}), nil
}

// DeleteSchedule deletes one of the caller's schedules and its runs.
func (h *scheduleHandler) DeleteSchedule(ctx context.Context, req *connect.Request[syncrv1.DeleteScheduleRequest]) (*connect.Response[syncrv1.DeleteScheduleResponse], error) {
	id, err := parseScheduleID(req.Msg.GetScheduleId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	if err := h.schedules.Delete(ctx, actor, id); err != nil {
		return nil, h.fail(ctx, syncrv1connect.ScheduleServiceDeleteScheduleProcedure, err)
	}
	return connect.NewResponse(&syncrv1.DeleteScheduleResponse{}), nil
}

// RunScheduleNow adds a manual run of one of the caller's schedules.
func (h *scheduleHandler) RunScheduleNow(ctx context.Context, req *connect.Request[syncrv1.RunScheduleNowRequest]) (*connect.Response[syncrv1.RunScheduleNowResponse], error) {
	id, err := parseScheduleID(req.Msg.GetScheduleId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.schedules.RunNow(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ScheduleServiceRunScheduleNowProcedure, err)
	}
	return connect.NewResponse(&syncrv1.RunScheduleNowResponse{Run: scheduleRunToProto(r)}), nil
}

// ListScheduleRuns returns a page of a schedule's runs, newest first.
func (h *scheduleHandler) ListScheduleRuns(ctx context.Context, req *connect.Request[syncrv1.ListScheduleRunsRequest]) (*connect.Response[syncrv1.ListScheduleRunsResponse], error) {
	const procedure = syncrv1connect.ScheduleServiceListScheduleRunsProcedure
	id, err := parseScheduleID(req.Msg.GetScheduleId())
	if err != nil {
		return nil, err
	}
	limit, err := clampPageSize(req.Msg.GetPageSize(), schedules.DefaultPageSize, schedules.MaxPageSize)
	if err != nil {
		return nil, invalid(err.Error())
	}
	before, err := decodeRunPageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, invalid(err.Error())
	}
	actor, _ := ActorFrom(ctx)
	page, err := h.schedules.Runs(ctx, actor, id, before, limit)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	resp := &syncrv1.ListScheduleRunsResponse{Runs: make([]*syncrv1.ScheduleRun, 0, len(page.Runs)), NextPageToken: encodeRunPageToken(page.Next)}
	for _, r := range page.Runs {
		resp.Runs = append(resp.Runs, scheduleRunToProto(r))
	}
	return connect.NewResponse(resp), nil
}

// PreviewSchedule checks an expression and time zone and returns their
// next occurrences.
func (h *scheduleHandler) PreviewSchedule(ctx context.Context, req *connect.Request[syncrv1.PreviewScheduleRequest]) (*connect.Response[syncrv1.PreviewScheduleResponse], error) {
	after, err := optTime("after", req.Msg.GetAfter())
	if err != nil {
		return nil, err
	}
	p, err := h.schedules.Preview(req.Msg.GetCron(), req.Msg.GetTimezone(), after, int(req.Msg.GetCount()))
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ScheduleServicePreviewScheduleProcedure, err)
	}
	resp := &syncrv1.PreviewScheduleResponse{Normalized: p.Normalized, NextRuns: make([]*timestamppb.Timestamp, 0, len(p.NextRuns))}
	for _, t := range p.NextRuns {
		resp.NextRuns = append(resp.NextRuns, timestamppb.New(t))
	}
	return connect.NewResponse(resp), nil
}

// input converts a ScheduleInput, checking its definition with the job
// kinds exactly as RunJob does. Malformed fields are a validation error
// with the proto field paths; the service checks the rest.
func (h *scheduleHandler) input(ctx context.Context, procedure string, m *syncrv1.ScheduleInput) (schedules.Input, error) {
	if h.kinds == nil {
		return schedules.Input{}, connect.NewError(connect.CodeUnimplemented, errors.New("this controller has no job kinds"))
	}
	if m == nil {
		return schedules.Input{}, invalidField("input", "is required")
	}
	var v domain.Validation
	in := schedules.Input{
		Name: m.GetName(), Description: m.GetDescription(), Cron: m.GetCron(), Timezone: m.GetTimezone(), Enabled: m.GetEnabled(),
	}
	switch t := m.GetTarget().GetTarget().(type) {
	case *syncrv1.ScheduleTarget_WorkerId:
		id, err := domain.ParseWorkerID(t.WorkerId)
		if err != nil || id.IsZero() {
			v.Add("target.worker_id", "is not a valid ID")
		}
		in.Target.WorkerID = id
	case *syncrv1.ScheduleTarget_WorkerLabel:
		if strings.TrimSpace(t.WorkerLabel) == "" {
			v.Add("target.worker_label", "is required")
		}
		in.Target.Label = strings.TrimSpace(t.WorkerLabel)
	}
	action, ok := missedActionFromProto(m.GetMissedRun().GetAction())
	if !ok {
		v.Add("missed_run.action", "is not a known missed-run action")
	}
	in.Missed.Action = action
	if d := m.GetMissedRun().GetMaxDelay(); d != nil {
		if d.CheckValid() != nil {
			v.Add("missed_run.max_delay", "is not a valid duration")
		} else {
			in.Missed.MaxDelay = d.AsDuration()
		}
	}
	if in.Overlap, ok = overlapFromProto(m.GetOverlap()); !ok {
		v.Add("overlap", "is not a known overlap policy")
	}
	if def := m.GetDefinition(); def == nil {
		v.Add("definition", "is required")
	} else {
		kind, refs, err := h.kinds.Check(def)
		var verr *domain.ValidationError
		switch {
		case errors.As(err, &verr):
			v.AddAll("", verr.Fields)
		case err != nil:
			return schedules.Input{}, h.fail(ctx, procedure, err)
		default:
			encoded, err := jobkind.Marshal(def)
			if err != nil {
				return schedules.Input{}, h.fail(ctx, procedure, err)
			}
			in.Kind, in.Definition, in.RemoteIDs, in.SearchTerms = kind, encoded, refs.RemoteIDs, refs.SearchTerms
		}
	}
	for i, c := range m.GetContacts() {
		id, err := domain.ParseContactID(c.GetContactId())
		if err != nil || id.IsZero() {
			v.Add(fmt.Sprintf("contacts[%d].contact_id", i), "is not a valid ID")
		}
		link := schedules.ContactLink{ContactID: id}
		for _, e := range c.GetEvents() {
			ev, ok := notificationEventFromProto(e)
			if !ok {
				v.Add(fmt.Sprintf("contacts[%d].events", i), "has an unknown event")
				break
			}
			link.Events = append(link.Events, ev)
		}
		in.Contacts = append(in.Contacts, link)
	}
	if err := v.Err(); err != nil {
		return schedules.Input{}, h.fail(ctx, procedure, err)
	}
	return in, nil
}

// protoSchedule converts one schedule (see protoSchedules).
func (h *scheduleHandler) protoSchedule(ctx context.Context, actor domain.Actor, sc schedules.Schedule) (*syncrv1.Schedule, error) {
	out, err := h.protoSchedules(ctx, actor, []schedules.Schedule{sc})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// protoSchedules converts ss with their references: one lookup each for
// the remotes' and contacts' names and for the last runs.
func (h *scheduleHandler) protoSchedules(ctx context.Context, actor domain.Actor, ss []schedules.Schedule) ([]*syncrv1.Schedule, error) {
	var refs scheduleRefs
	var remoteIDs []domain.RemoteID
	var contactIDs []domain.ContactID
	ids := make([]domain.ScheduleID, 0, len(ss))
	for _, sc := range ss {
		ids = append(ids, sc.ID)
		for _, id := range sc.RemoteIDs {
			if !slices.Contains(remoteIDs, id) {
				remoteIDs = append(remoteIDs, id)
			}
		}
		for _, l := range sc.Contacts {
			if !slices.Contains(contactIDs, l.ContactID) {
				contactIDs = append(contactIDs, l.ContactID)
			}
		}
	}
	var err error
	if h.remotes != nil && len(remoteIDs) > 0 {
		if refs.remotes, err = h.remotes.Names(ctx, remoteIDs); err != nil {
			return nil, fmt.Errorf("name the remotes of schedules: %w", err)
		}
	}
	if h.contacts != nil && len(contactIDs) > 0 {
		if refs.contacts, err = h.contacts.Names(ctx, contactIDs); err != nil {
			return nil, fmt.Errorf("name the contacts of schedules: %w", err)
		}
	}
	if refs.last, err = h.schedules.LatestRuns(ctx, actor, ids); err != nil {
		return nil, err
	}
	out := make([]*syncrv1.Schedule, 0, len(ss))
	for _, sc := range ss {
		msg, err := scheduleToProto(sc, refs)
		if err != nil {
			// Only the definition is affected; the rest is still worth
			// showing.
			h.errs.log.WarnContext(ctx, "schedule definition is not decodable", "schedule_id", sc.ID.String(), "error", err.Error())
		}
		out = append(out, msg)
	}
	return out, nil
}

// parseScheduleID parses a request's schedule ID into an InvalidArgument
// error naming the field.
func parseScheduleID(s string) (domain.ScheduleID, error) {
	if s == "" {
		return domain.ScheduleID{}, invalidField("schedule_id", "is required")
	}
	id, err := domain.ParseScheduleID(s)
	if err != nil || id.IsZero() {
		return domain.ScheduleID{}, invalidField("schedule_id", "is not a valid ID")
	}
	return id, nil
}

// runTokenPrefix marks run page tokens (job tokens use their own).
const runTokenPrefix = "r1:"

// encodeRunPageToken makes the opaque token of the page after run next.
func encodeRunPageToken(next domain.ScheduleRunID) string {
	if next.IsZero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(runTokenPrefix + next.String()))
}

// decodeRunPageToken is the inverse of encodeRunPageToken; "" is the first
// page.
func decodeRunPageToken(token string) (domain.ScheduleRunID, error) {
	if token == "" {
		return domain.ScheduleRunID{}, nil
	}
	malformed := errors.New("page_token is malformed")
	if len(token) > maxPageToken {
		return domain.ScheduleRunID{}, malformed
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return domain.ScheduleRunID{}, malformed
	}
	rest, ok := strings.CutPrefix(string(raw), runTokenPrefix)
	if !ok {
		return domain.ScheduleRunID{}, malformed
	}
	id, err := domain.ParseScheduleRunID(rest)
	if err != nil || id.IsZero() {
		return domain.ScheduleRunID{}, malformed
	}
	return id, nil
}
