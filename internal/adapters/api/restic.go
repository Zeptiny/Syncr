package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// resticHandler implements syncr.v1.ResticService over the restic use
// cases (Deps.Restic), naming the repositories' remotes (Deps.Remotes) and
// the worker of a data deletion (Deps.Workers); without them the
// references have no names.
//
// The repository password is write-only: requests carry it and are never
// logged, and no response has a field for it.
type resticHandler struct {
	syncrv1connect.UnimplementedResticServiceHandler

	restic  ResticService
	remotes RemoteNames
	workers WorkerNames
	errs    errorMapper
}

var _ syncrv1connect.ResticServiceHandler = (*resticHandler)(nil)

// WorkerNames looks up a worker for its name (WorkerService.Get).
type WorkerNames interface {
	Get(ctx context.Context, id domain.WorkerID) (workers.Worker, error)
}

// fail maps an error of the restic use cases. A taken name, location or
// restic repository is AlreadyExists with a message saying which.
func (h *resticHandler) fail(ctx context.Context, procedure string, err error) error {
	switch {
	case errors.Is(err, restic.ErrNameTaken):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("a repository with this name already exists"))
	case errors.Is(err, restic.ErrLocationTaken):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("a repository at this location already exists"))
	case errors.Is(err, restic.ErrSameRepository):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("this restic repository is already one of your repositories"))
	}
	return h.errs.toConnect(ctx, procedure, "repository", err)
}

// parseRepositoryID parses a request's repository ID into an
// InvalidArgument error naming field.
func parseRepositoryID(field, s string) (domain.ResticRepoID, error) {
	if s == "" {
		return domain.ResticRepoID{}, invalidField(field, "is required")
	}
	id, err := domain.ParseResticRepoID(s)
	if err != nil || id.IsZero() {
		return domain.ResticRepoID{}, invalidField(field, "is not a valid ID")
	}
	return id, nil
}

// protoRepositories converts rs with the names of their remotes (one
// lookup for all).
func (h *resticHandler) protoRepositories(ctx context.Context, rs []restic.Repo) ([]*syncrv1.Repository, error) {
	var refs map[domain.RemoteID]remotes.Ref
	if h.remotes != nil && len(rs) > 0 {
		var ids []domain.RemoteID
		for _, r := range rs {
			if !slices.Contains(ids, r.RemoteID) {
				ids = append(ids, r.RemoteID)
			}
		}
		var err error
		if refs, err = h.remotes.Names(ctx, ids); err != nil {
			return nil, fmt.Errorf("name the remotes of repositories: %w", err)
		}
	}
	out := make([]*syncrv1.Repository, 0, len(rs))
	for _, r := range rs {
		out = append(out, repositoryToProto(r, refs[r.RemoteID]))
	}
	return out, nil
}

func (h *resticHandler) protoRepository(ctx context.Context, r restic.Repo) (*syncrv1.Repository, error) {
	out, err := h.protoRepositories(ctx, []restic.Repo{r})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// ListRepositories lists the caller's repositories, by name.
func (h *resticHandler) ListRepositories(ctx context.Context, req *connect.Request[syncrv1.ListRepositoriesRequest]) (*connect.Response[syncrv1.ListRepositoriesResponse], error) {
	const procedure = syncrv1connect.ResticServiceListRepositoriesProcedure
	var f restic.ListFilter
	if s := req.Msg.GetRemoteId(); s != "" {
		id, err := parseRemoteID("remote_id", s)
		if err != nil {
			return nil, err
		}
		f.RemoteID = id
	}
	actor, _ := ActorFrom(ctx)
	list, err := h.restic.List(ctx, actor, f)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msgs, err := h.protoRepositories(ctx, list)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.ListRepositoriesResponse{Repositories: msgs}), nil
}

// GetRepository returns one of the caller's repositories.
func (h *resticHandler) GetRepository(ctx context.Context, req *connect.Request[syncrv1.GetRepositoryRequest]) (*connect.Response[syncrv1.GetRepositoryResponse], error) {
	const procedure = syncrv1connect.ResticServiceGetRepositoryProcedure
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.restic.Get(ctx, actor, id)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoRepository(ctx, r)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.GetRepositoryResponse{Repository: msg}), nil
}

// CreateRepository initializes or connects to a repository on a worker and
// stores it for the caller.
func (h *resticHandler) CreateRepository(ctx context.Context, req *connect.Request[syncrv1.CreateRepositoryRequest]) (*connect.Response[syncrv1.CreateRepositoryResponse], error) {
	const procedure = syncrv1connect.ResticServiceCreateRepositoryProcedure
	m := req.Msg.GetInput()
	if m == nil {
		return nil, invalidField("input", "is required")
	}
	in := restic.Input{
		Name: m.GetName(), Description: m.GetDescription(), Path: m.GetPath(), Password: m.GetPassword(),
		Mode: repositoryModeFromProto(m.GetMode()),
	}
	var v domain.Validation
	if s := m.GetRemoteId(); s != "" {
		id, err := domain.ParseRemoteID(s)
		if err != nil || id.IsZero() {
			v.Add("remote_id", "is not a valid ID")
		}
		in.RemoteID = id
	}
	if s := m.GetWorkerId(); s != "" {
		id, err := domain.ParseWorkerID(s)
		if err != nil || id.IsZero() {
			v.Add("worker_id", "is not a valid ID")
		}
		in.WorkerID = id
	}
	if err := v.Err(); err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	actor, _ := ActorFrom(ctx)
	created, err := h.restic.Create(ctx, actor, in)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoRepository(ctx, created.Repo)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.CreateRepositoryResponse{Repository: msg, Worker: probeWorkerRef(created.WorkerID, created.WorkerName)}), nil
}

// UpdateRepository renames one of the caller's repositories or replaces
// its stored password (version-checked).
func (h *resticHandler) UpdateRepository(ctx context.Context, req *connect.Request[syncrv1.UpdateRepositoryRequest]) (*connect.Response[syncrv1.UpdateRepositoryResponse], error) {
	const procedure = syncrv1connect.ResticServiceUpdateRepositoryProcedure
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetVersion() < 1 {
		return nil, invalidField("version", "is required (the version of the repository that was read)")
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	r, err := h.restic.Update(ctx, actor, id, req.Msg.GetVersion(), restic.Update{
		Name: req.Msg.GetName(), Description: req.Msg.GetDescription(), Password: req.Msg.GetPassword(), WorkerID: workerID,
	})
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	msg, err := h.protoRepository(ctx, r)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	return connect.NewResponse(&syncrv1.UpdateRepositoryResponse{Repository: msg}), nil
}

// DeleteRepository forgets one of the caller's repositories or, with
// delete_data and the typed name, starts the job that deletes its data
// first.
func (h *resticHandler) DeleteRepository(ctx context.Context, req *connect.Request[syncrv1.DeleteRepositoryRequest]) (*connect.Response[syncrv1.DeleteRepositoryResponse], error) {
	const procedure = syncrv1connect.ResticServiceDeleteRepositoryProcedure
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	res, err := h.restic.Delete(ctx, actor, id, restic.DeleteOptions{
		DeleteData: req.Msg.GetDeleteData(), ConfirmName: req.Msg.GetConfirmName(), WorkerID: workerID,
	})
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	out := &syncrv1.DeleteRepositoryResponse{}
	if !res.DeletionJobID.IsZero() {
		out.DeletionJobId = res.DeletionJobID.String()
	}
	if !res.WorkerID.IsZero() {
		name := ""
		if h.workers != nil {
			// The name is for display only: the deletion has started.
			if w, err := h.workers.Get(ctx, res.WorkerID); err == nil {
				name = w.Name
			}
		}
		out.Worker = probeWorkerRef(res.WorkerID, name)
	}
	return connect.NewResponse(out), nil
}

// ListSnapshots returns the cached snapshot list of one of the caller's
// repositories, refreshed through a worker when asked or never listed.
func (h *resticHandler) ListSnapshots(ctx context.Context, req *connect.Request[syncrv1.ListSnapshotsRequest]) (*connect.Response[syncrv1.ListSnapshotsResponse], error) {
	const procedure = syncrv1connect.ResticServiceListSnapshotsProcedure
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	list, err := h.restic.Snapshots(ctx, actor, id, restic.SnapshotsOptions{Refresh: req.Msg.GetRefresh(), WorkerID: workerID})
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	out := &syncrv1.ListSnapshotsResponse{
		Snapshots:    make([]*syncrv1.ResticSnapshot, 0, len(list.Snapshots)),
		RefreshedAt:  ts(list.RefreshedAt),
		Stale:        list.Stale,
		Truncated:    list.Truncated,
		RefreshError: list.RefreshError,
		Worker:       probeWorkerRef(list.WorkerID, list.WorkerName),
	}
	for _, s := range list.Snapshots {
		out.Snapshots = append(out.Snapshots, snapshotToProto(s))
	}
	return connect.NewResponse(out), nil
}

// BrowseSnapshot lists one directory of a snapshot of one of the caller's
// repositories.
func (h *resticHandler) BrowseSnapshot(ctx context.Context, req *connect.Request[syncrv1.BrowseSnapshotRequest]) (*connect.Response[syncrv1.BrowseSnapshotResponse], error) {
	const procedure = syncrv1connect.ResticServiceBrowseSnapshotProcedure
	id, err := parseRepositoryID("repository_id", req.Msg.GetRepositoryId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	l, err := h.restic.Browse(ctx, actor, id, req.Msg.GetSnapshotId(), req.Msg.GetPath(), workerID)
	if err != nil {
		return nil, h.fail(ctx, procedure, err)
	}
	out := &syncrv1.BrowseSnapshotResponse{
		Path:      l.Path,
		Entries:   make([]*syncrv1.ProbeEntry, 0, len(l.Entries)),
		Truncated: l.Truncated,
		Worker:    probeWorkerRef(l.WorkerID, l.WorkerName),
	}
	for _, e := range l.Entries {
		out.Entries = append(out.Entries, entryToProto(e))
	}
	return connect.NewResponse(out), nil
}
