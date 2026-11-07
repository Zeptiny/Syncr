package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/backend"
)

// remoteHandler implements syncr.v1.RemoteService. Secret values are never
// part of a response or a log line.
//
// m3: pending track B: every RPC is Unimplemented until then.
type remoteHandler struct {
	syncrv1connect.UnimplementedRemoteServiceHandler

	remotes  RemoteService
	backends *backend.Registry
	errs     errorMapper
}

var _ syncrv1connect.RemoteServiceHandler = (*remoteHandler)(nil)
