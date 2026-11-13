package api

import (
	"context"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Milestone 7: measuring a worker's folder or a remote's, and a remote's
// usage. Any signed-in user may call them; remotes stay the caller's own
// through the probe use cases, which also rate limit them. The measuring
// calls wait for one heavy probe and accept no GET (no NO_SIDE_EFFECTS
// option); GetRemoteUsage reads only and accepts GET.

// MeasureWorkerFolder measures a folder of a worker's filesystem. Any user
// may measure any worker's folders. Not implemented yet: a well-formed
// request is Unimplemented.
func (h *probeHandler) MeasureWorkerFolder(_ context.Context, req *connect.Request[syncrv1.MeasureWorkerFolderRequest]) (*connect.Response[syncrv1.MeasureWorkerFolderResponse], error) {
	if req.Msg.GetWorkerId() == "" {
		return nil, invalidField("worker_id", "is required")
	}
	if _, err := optionalWorkerID(req.Msg.GetWorkerId()); err != nil {
		return nil, err
	}
	return nil, notAvailable("measuring folders")
}

// MeasureRemoteFolder measures a folder of one of the caller's remotes.
// Not implemented yet: a well-formed request is Unimplemented.
func (h *probeHandler) MeasureRemoteFolder(_ context.Context, req *connect.Request[syncrv1.MeasureRemoteFolderRequest]) (*connect.Response[syncrv1.MeasureRemoteFolderResponse], error) {
	if _, err := parseRemoteID("remote_id", req.Msg.GetRemoteId()); err != nil {
		return nil, err
	}
	if _, err := optionalWorkerID(req.Msg.GetWorkerId()); err != nil {
		return nil, err
	}
	return nil, notAvailable("measuring folders")
}

// GetRemoteUsage asks one of the caller's remotes for its usage. Not
// implemented yet: a well-formed request is Unimplemented.
func (h *probeHandler) GetRemoteUsage(_ context.Context, req *connect.Request[syncrv1.GetRemoteUsageRequest]) (*connect.Response[syncrv1.GetRemoteUsageResponse], error) {
	if _, err := parseRemoteID("remote_id", req.Msg.GetRemoteId()); err != nil {
		return nil, err
	}
	if _, err := optionalWorkerID(req.Msg.GetWorkerId()); err != nil {
		return nil, err
	}
	return nil, notAvailable("a remote's usage")
}
