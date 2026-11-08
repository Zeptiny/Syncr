package api

import (
	"context"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/backend"
)

// backendHandler implements syncr.v1.BackendService: the descriptors of
// Deps.Backends, static for a controller build.
type backendHandler struct {
	syncrv1connect.UnimplementedBackendServiceHandler

	backends *backend.Registry
}

var _ syncrv1connect.BackendServiceHandler = (*backendHandler)(nil)

// ListBackends returns every backend type, by label. How a type maps to
// rclone (option names, obscured secrets) is not part of a descriptor's
// wire form.
func (h *backendHandler) ListBackends(context.Context, *connect.Request[syncrv1.ListBackendsRequest]) (*connect.Response[syncrv1.ListBackendsResponse], error) {
	types := h.backends.Types()
	out := &syncrv1.ListBackendsResponse{Backends: make([]*syncrv1.Backend, 0, len(types))}
	for _, t := range types {
		out.Backends = append(out.Backends, backendToProto(t))
	}
	return connect.NewResponse(out), nil
}

func backendToProto(t backend.Type) *syncrv1.Backend {
	caps := t.Capabilities()
	fields := t.Fields()
	out := &syncrv1.Backend{
		Id:          t.ID(),
		Label:       t.Label(),
		Description: t.Description(),
		Fields:      make([]*syncrv1.BackendField, 0, len(fields)),
		Capabilities: &syncrv1.BackendCapabilities{
			Local: caps.Local, Union: caps.Union, UnionMember: caps.UnionMember, ResticRepository: caps.ResticRepository,
		},
	}
	for _, f := range fields {
		out.Fields = append(out.Fields, fieldToProto(f.Descriptor()))
	}
	return out
}
