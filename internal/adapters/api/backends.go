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
		bf := &syncrv1.BackendField{
			Name:         f.Name,
			Label:        f.Label,
			Help:         f.Help,
			Placeholder:  f.Placeholder,
			Kind:         fieldKindToProto(f.Kind),
			Required:     f.Required,
			Secret:       f.Secret,
			Advanced:     f.Advanced,
			DefaultValue: f.Default,
			Choices:      make([]*syncrv1.FieldChoice, 0, len(f.Choices)),
		}
		for _, c := range f.Choices {
			bf.Choices = append(bf.Choices, &syncrv1.FieldChoice{Value: c.Value, Label: c.Label})
		}
		out.Fields = append(out.Fields, bf)
	}
	return out
}

func fieldKindToProto(k backend.Kind) syncrv1.FieldKind {
	switch k {
	case backend.KindString:
		return syncrv1.FieldKind_FIELD_KIND_STRING
	case backend.KindText:
		return syncrv1.FieldKind_FIELD_KIND_TEXT
	case backend.KindInt:
		return syncrv1.FieldKind_FIELD_KIND_INT
	case backend.KindBool:
		return syncrv1.FieldKind_FIELD_KIND_BOOL
	case backend.KindEnum:
		return syncrv1.FieldKind_FIELD_KIND_ENUM
	case backend.KindMembers:
		return syncrv1.FieldKind_FIELD_KIND_MEMBERS
	}
	return syncrv1.FieldKind_FIELD_KIND_UNSPECIFIED
}
