package api

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// remoteHandler implements syncr.v1.RemoteService. Secret values are never
// part of a response or a log line: responses carry only the names of the
// secrets that are set, and requests (which carry secrets) are never
// logged.
type remoteHandler struct {
	syncrv1connect.UnimplementedRemoteServiceHandler

	remotes  RemoteService
	backends *backend.Registry
	errs     errorMapper
}

var _ syncrv1connect.RemoteServiceHandler = (*remoteHandler)(nil)

func (h *remoteHandler) fail(ctx context.Context, procedure string, err error) error {
	return h.errs.toConnect(ctx, procedure, "remote", err)
}

// ListRemotes lists the caller's remotes, by name. used_by_unions comes
// from the same list: a union and its members always have the same owner.
func (h *remoteHandler) ListRemotes(ctx context.Context, req *connect.Request[syncrv1.ListRemotesRequest]) (*connect.Response[syncrv1.ListRemotesResponse], error) {
	actor, _ := ActorFrom(ctx)
	all, err := h.remotes.List(ctx, actor, remotes.ListFilter{})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.RemoteServiceListRemotesProcedure, err)
	}
	usedBy := map[domain.RemoteID][]domain.RemoteID{}
	for _, r := range all {
		for _, m := range r.Members {
			if !slices.Contains(usedBy[m.RemoteID], r.ID) {
				usedBy[m.RemoteID] = append(usedBy[m.RemoteID], r.ID)
			}
		}
	}
	out := &syncrv1.ListRemotesResponse{Remotes: []*syncrv1.Remote{}}
	for _, r := range all {
		if t := req.Msg.GetBackendType(); t == "" || r.Type == t {
			out.Remotes = append(out.Remotes, remoteToProto(r, usedBy[r.ID]))
		}
	}
	return connect.NewResponse(out), nil
}

// GetRemote returns one of the caller's remotes.
func (h *remoteHandler) GetRemote(ctx context.Context, req *connect.Request[syncrv1.GetRemoteRequest]) (*connect.Response[syncrv1.GetRemoteResponse], error) {
	const procedure = syncrv1connect.RemoteServiceGetRemoteProcedure
	id, err := parseRemoteID("remote_id", req.Msg.GetRemoteId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.remotes.Get(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.remote(ctx, actor, r)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.GetRemoteResponse{Remote: msg}), nil
}

// CreateRemote creates a remote for the caller.
func (h *remoteHandler) CreateRemote(ctx context.Context, req *connect.Request[syncrv1.CreateRemoteRequest]) (*connect.Response[syncrv1.CreateRemoteResponse], error) {
	in, err := remoteInputFromProto(req.Msg.GetInput())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.RemoteServiceCreateRemoteProcedure, err)
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.remotes.Create(ctx, actor, in)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.RemoteServiceCreateRemoteProcedure, err)
	}
	return connect.NewResponse(&syncrv1.CreateRemoteResponse{Remote: remoteToProto(r, nil)}), nil
}

// UpdateRemote changes one of the caller's remotes.
func (h *remoteHandler) UpdateRemote(ctx context.Context, req *connect.Request[syncrv1.UpdateRemoteRequest]) (*connect.Response[syncrv1.UpdateRemoteResponse], error) {
	const procedure = syncrv1connect.RemoteServiceUpdateRemoteProcedure
	id, err := parseRemoteID("remote_id", req.Msg.GetRemoteId())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetVersion() < 1 {
		return nil, invalidField("version", "is required (the version of the remote that was read)")
	}
	in, err := remoteInputFromProto(req.Msg.GetInput())
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.remotes.Update(ctx, actor, id, req.Msg.GetVersion(), in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.remote(ctx, actor, r)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.UpdateRemoteResponse{Remote: msg}), nil
}

// DeleteRemote deletes one of the caller's remotes.
func (h *remoteHandler) DeleteRemote(ctx context.Context, req *connect.Request[syncrv1.DeleteRemoteRequest]) (*connect.Response[syncrv1.DeleteRemoteResponse], error) {
	id, err := parseRemoteID("remote_id", req.Msg.GetRemoteId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	if err := h.remotes.Delete(ctx, actor, id); err != nil {
		return nil, h.fail(ctx, syncrv1connect.RemoteServiceDeleteRemoteProcedure, err)
	}
	return connect.NewResponse(&syncrv1.DeleteRemoteResponse{}), nil
}

// remote converts r with the unions of actor that use it.
func (h *remoteHandler) remote(ctx context.Context, actor domain.Actor, r remotes.Remote) (*syncrv1.Remote, error) {
	unions, err := h.remotes.UnionsUsing(ctx, actor, r.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]domain.RemoteID, len(unions))
	for i, u := range unions {
		ids[i] = u.ID
	}
	return remoteToProto(r, ids), nil
}

// parseRemoteID parses a request's remote ID into an InvalidArgument error
// naming field.
func parseRemoteID(field, s string) (domain.RemoteID, error) {
	if s == "" {
		return domain.RemoteID{}, invalidField(field, "is required")
	}
	id, err := domain.ParseRemoteID(s)
	if err != nil || id.IsZero() {
		return domain.RemoteID{}, invalidField(field, "is not a valid ID")
	}
	return id, nil
}

// remoteToProto converts r. It carries the names of the secrets that are
// set, never their values (Remote has none: they stay encrypted).
func remoteToProto(r remotes.Remote, usedBy []domain.RemoteID) *syncrv1.Remote {
	out := &syncrv1.Remote{
		Id:           r.ID.String(),
		Name:         r.Name,
		BackendType:  r.Type,
		Options:      maps.Clone(r.Options),
		SecretsSet:   slices.Clone(r.SecretKeys),
		CreatedAt:    ts(r.CreatedAt),
		UpdatedAt:    ts(r.UpdatedAt),
		Version:      r.Version,
		Members:      make([]*syncrv1.UnionMember, 0, len(r.Members)),
		UsedByUnions: make([]string, 0, len(usedBy)),
	}
	for _, m := range r.Members {
		out.Members = append(out.Members, &syncrv1.UnionMember{RemoteId: m.RemoteID.String(), Path: m.Path, Mode: unionModeToProto(m.Mode)})
	}
	for _, id := range usedBy {
		out.UsedByUnions = append(out.UsedByUnions, id.String())
	}
	return out
}

// remoteInputFromProto converts a RemoteInput (nil is empty). Malformed
// members are reported as a *domain.ValidationError.
func remoteInputFromProto(in *syncrv1.RemoteInput) (remotes.Input, error) {
	out := remotes.Input{
		Name:         in.GetName(),
		Type:         in.GetBackendType(),
		Options:      maps.Clone(in.GetOptions()),
		Secrets:      maps.Clone(in.GetSecrets()),
		ClearSecrets: slices.Clone(in.GetClearSecrets()),
	}
	var v domain.Validation
	for i, m := range in.GetMembers() {
		member := remotes.Member{Path: m.GetPath()}
		if m.GetRemoteId() != "" {
			id, err := domain.ParseRemoteID(m.GetRemoteId())
			if err != nil {
				v.Add(fmt.Sprintf("members[%d].remote_id", i), "is not a valid ID")
			}
			member.RemoteID = id
		}
		mode, ok := unionModeFromProto(m.GetMode())
		if !ok {
			v.Add(fmt.Sprintf("members[%d].mode", i), "is not a known mode")
		}
		member.Mode = mode
		out.Members = append(out.Members, member)
	}
	return out, v.Err()
}

func unionModeToProto(m backend.UnionMode) syncrv1.UnionUpstreamMode {
	switch m {
	case backend.UnionReadOnly:
		return syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY
	case backend.UnionNoCreate:
		return syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE
	default:
		return syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE
	}
}

// unionModeFromProto maps a mode; UNSPECIFIED means read-write.
func unionModeFromProto(m syncrv1.UnionUpstreamMode) (backend.UnionMode, bool) {
	switch m {
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_UNSPECIFIED, syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE:
		return backend.UnionReadWrite, true
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY:
		return backend.UnionReadOnly, true
	case syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE:
		return backend.UnionNoCreate, true
	}
	return "", false
}
