package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// probeHandler implements syncr.v1.ProbeService over the probe use cases:
// browsing a worker's folders, browsing a remote and testing a remote,
// each through a worker. Requests may carry secrets (a remote being
// tested) and are never logged; responses carry none.
type probeHandler struct {
	syncrv1connect.UnimplementedProbeServiceHandler

	probes ProbeService
	errs   errorMapper
}

var _ syncrv1connect.ProbeServiceHandler = (*probeHandler)(nil)

func (h *probeHandler) fail(ctx context.Context, procedure, what string, err error) error {
	return h.errs.toConnect(ctx, procedure, what, err)
}

// optionalWorkerID parses a request's worker_id; empty is the zero ID
// (the controller chooses).
func optionalWorkerID(s string) (domain.WorkerID, error) {
	if s == "" {
		return domain.WorkerID{}, nil
	}
	id, err := domain.ParseWorkerID(s)
	if err != nil || id.IsZero() {
		return domain.WorkerID{}, invalidField("worker_id", "is not a valid ID")
	}
	return id, nil
}

// ListWorkerFiles lists a folder of a worker's filesystem (its local roots
// for an empty path). Any user may browse any worker.
func (h *probeHandler) ListWorkerFiles(ctx context.Context, req *connect.Request[syncrv1.ListWorkerFilesRequest]) (*connect.Response[syncrv1.ListWorkerFilesResponse], error) {
	actor, _ := ActorFrom(ctx)
	if req.Msg.GetWorkerId() == "" {
		return nil, invalidField("worker_id", "is required")
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	out, err := h.probes.ListWorkerFiles(ctx, actor, workerID, req.Msg.GetPath(), req.Msg.GetDirsOnly())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ProbeServiceListWorkerFilesProcedure, "worker", err)
	}
	return connect.NewResponse(out), nil
}

// ListRemoteFiles lists a folder of one of the caller's remotes.
func (h *probeHandler) ListRemoteFiles(ctx context.Context, req *connect.Request[syncrv1.ListRemoteFilesRequest]) (*connect.Response[syncrv1.ListRemoteFilesResponse], error) {
	actor, _ := ActorFrom(ctx)
	remoteID, err := parseRemoteID("remote_id", req.Msg.GetRemoteId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	out, err := h.probes.ListRemoteFiles(ctx, actor, remoteID, req.Msg.GetPath(), workerID, req.Msg.GetDirsOnly())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ProbeServiceListRemoteFilesProcedure, "remote or worker", err)
	}
	return connect.NewResponse(out), nil
}

// TestRemote tests a stored remote, a stored remote with unsaved values,
// or an unsaved new remote. What the worker reports is in the response.
func (h *probeHandler) TestRemote(ctx context.Context, req *connect.Request[syncrv1.TestRemoteRequest]) (*connect.Response[syncrv1.TestRemoteResponse], error) {
	actor, _ := ActorFrom(ctx)
	var (
		remoteID domain.RemoteID
		in       *remotes.Input
		err      error
	)
	switch {
	case req.Msg.GetRemoteId() != "":
		if remoteID, err = parseRemoteID("remote_id", req.Msg.GetRemoteId()); err != nil {
			return nil, err
		}
	case req.Msg.GetInput() == nil:
		return nil, invalidField("remote_id", "is required unless input describes a new remote")
	}
	if req.Msg.GetInput() != nil {
		input, err := remoteInputFromProto(req.Msg.GetInput())
		if err != nil {
			return nil, h.fail(ctx, syncrv1connect.ProbeServiceTestRemoteProcedure, "remote", err)
		}
		in = &input
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	out, err := h.probes.TestRemote(ctx, actor, remoteID, in, req.Msg.GetPath(), workerID)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ProbeServiceTestRemoteProcedure, "remote or worker", err)
	}
	return connect.NewResponse(out), nil
}

// GetSftpHostKeys fetches an SFTP server's host keys through a worker. It
// is not implemented yet: it answers Unimplemented until the probe use
// cases can run the sftp_host_keys probe.
func (h *probeHandler) GetSftpHostKeys(context.Context, *connect.Request[syncrv1.GetSftpHostKeysRequest]) (*connect.Response[syncrv1.GetSftpHostKeysResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("fetching SFTP host keys is not implemented yet"))
}
