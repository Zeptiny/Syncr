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

// ListRemotes lists the caller's remotes, by name. used_by_unions and
// wrapped_by come from the same list: a union and its members, and a
// wrapper and its wrapped remote, always have the same owner.
func (h *remoteHandler) ListRemotes(ctx context.Context, req *connect.Request[syncrv1.ListRemotesRequest]) (*connect.Response[syncrv1.ListRemotesResponse], error) {
	actor, _ := ActorFrom(ctx)
	all, err := h.remotes.List(ctx, actor, remotes.ListFilter{})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.RemoteServiceListRemotesProcedure, err)
	}
	using := map[domain.RemoteID][]remotes.Remote{}
	for _, r := range all {
		for _, id := range usedRemotes(r) {
			using[id] = append(using[id], r)
		}
	}
	out := &syncrv1.ListRemotesResponse{Remotes: []*syncrv1.Remote{}}
	for _, r := range all {
		if t := req.Msg.GetBackendType(); t == "" || r.Type == t {
			out.Remotes = append(out.Remotes, remoteToProto(r, using[r.ID]))
		}
	}
	return connect.NewResponse(out), nil
}

// usedRemotes returns the remotes r uses, each once: its members and its
// wrapped remote.
func usedRemotes(r remotes.Remote) []domain.RemoteID {
	var out []domain.RemoteID
	for _, m := range r.Members {
		if !slices.Contains(out, m.RemoteID) {
			out = append(out, m.RemoteID)
		}
	}
	if w := r.Wrapped.RemoteID; !w.IsZero() && !slices.Contains(out, w) {
		out = append(out, w)
	}
	return out
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

// remote converts r with the remotes of actor that use it.
func (h *remoteHandler) remote(ctx context.Context, actor domain.Actor, r remotes.Remote) (*syncrv1.Remote, error) {
	using, err := h.remotes.RemotesUsing(ctx, actor, r.ID)
	if err != nil {
		return nil, err
	}
	return remoteToProto(r, using), nil
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

// remoteToProto converts r with the remotes that use it, in their order:
// those that wrap it (wrapped_by) and the others, its unions
// (used_by_unions). It carries the names of the secrets that are set,
// never their values (Remote has none: they stay encrypted).
func remoteToProto(r remotes.Remote, using []remotes.Remote) *syncrv1.Remote {
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
		UsedByUnions: []string{},
		WrappedBy:    []string{},
	}
	for _, m := range r.Members {
		out.Members = append(out.Members, &syncrv1.UnionMember{RemoteId: m.RemoteID.String(), Path: m.Path, Mode: unionModeToProto(m.Mode)})
	}
	if !r.Wrapped.IsZero() {
		out.Wrapped = &syncrv1.WrappedRemote{RemoteId: r.Wrapped.RemoteID.String(), Path: r.Wrapped.Path}
	}
	for _, u := range using {
		if u.Wrapped.RemoteID == r.ID {
			out.WrappedBy = append(out.WrappedBy, u.ID.String())
		} else {
			out.UsedByUnions = append(out.UsedByUnions, u.ID.String())
		}
	}
	return out
}

// remoteInputFromProto converts a RemoteInput (nil is empty). Malformed
// members and a malformed wrapped remote ID are reported as a
// *domain.ValidationError.
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
	if w := in.GetWrapped(); w != nil {
		out.Wrapped.Path = w.GetPath()
		if w.GetRemoteId() != "" {
			id, err := domain.ParseRemoteID(w.GetRemoteId())
			if err != nil {
				v.Add("wrapped.remote_id", "is not a valid ID")
			}
			out.Wrapped.RemoteID = id
		}
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
