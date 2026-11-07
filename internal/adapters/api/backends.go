package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/backend"
)

// backendHandler implements syncr.v1.BackendService: the descriptors of
// Deps.Backends, static for a controller build.
//
// m3: pending track B: ListBackends is Unimplemented until then.
type backendHandler struct {
	syncrv1connect.UnimplementedBackendServiceHandler

	backends *backend.Registry
}

var _ syncrv1connect.BackendServiceHandler = (*backendHandler)(nil)
