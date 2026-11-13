package api

import (
	"context"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// Milestone 7: measuring a worker's folder or a remote's, and a remote's
// usage. Any signed-in user may call them; remotes stay the caller's own
// through the probe use cases, which also rate limit them. The measuring
// calls wait for one heavy probe and accept no GET (no NO_SIDE_EFFECTS
// option); GetRemoteUsage reads only and accepts GET.

// MeasureWorkerFolder measures a folder of a worker's filesystem. Any user
// may measure any worker's folders.
func (h *probeHandler) MeasureWorkerFolder(ctx context.Context, req *connect.Request[syncrv1.MeasureWorkerFolderRequest]) (*connect.Response[syncrv1.MeasureWorkerFolderResponse], error) {
	actor, _ := ActorFrom(ctx)
	if req.Msg.GetWorkerId() == "" {
		return nil, invalidField("worker_id", "is required")
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	out, err := h.probes.MeasureWorkerFolder(ctx, actor, workerID, req.Msg.GetPath())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ProbeServiceMeasureWorkerFolderProcedure, "worker", err)
	}
	return connect.NewResponse(out), nil
}

// MeasureRemoteFolder measures a folder of one of the caller's remotes.
func (h *probeHandler) MeasureRemoteFolder(ctx context.Context, req *connect.Request[syncrv1.MeasureRemoteFolderRequest]) (*connect.Response[syncrv1.MeasureRemoteFolderResponse], error) {
	actor, _ := ActorFrom(ctx)
	remoteID, err := parseRemoteID("remote_id", req.Msg.GetRemoteId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	out, err := h.probes.MeasureRemoteFolder(ctx, actor, remoteID, req.Msg.GetPath(), workerID)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ProbeServiceMeasureRemoteFolderProcedure, "remote or worker", err)
	}
	return connect.NewResponse(out), nil
}

// GetRemoteUsage asks one of the caller's remotes for its usage.
func (h *probeHandler) GetRemoteUsage(ctx context.Context, req *connect.Request[syncrv1.GetRemoteUsageRequest]) (*connect.Response[syncrv1.GetRemoteUsageResponse], error) {
	actor, _ := ActorFrom(ctx)
	remoteID, err := parseRemoteID("remote_id", req.Msg.GetRemoteId())
	if err != nil {
		return nil, err
	}
	workerID, err := optionalWorkerID(req.Msg.GetWorkerId())
	if err != nil {
		return nil, err
	}
	out, err := h.probes.RemoteUsage(ctx, actor, remoteID, workerID)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.ProbeServiceGetRemoteUsageProcedure, "remote or worker", err)
	}
	return connect.NewResponse(out), nil
}
