package api

import (
	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/descriptor"
)

// fieldToProto converts a form field descriptor, of a backend type
// (backend.Field.Descriptor) or a notification channel, to its wire form.
// BackendService and ChannelService both serve it.
func fieldToProto(f descriptor.Field) *syncrv1.BackendField {
	out := &syncrv1.BackendField{
		Name:         f.Name,
		Label:        f.Label,
		Help:         f.Help,
		Placeholder:  f.Placeholder,
		Kind:         fieldKindToProto(f.Kind),
		Required:     f.Required,
		Secret:       f.Secret,
		Advanced:     f.Advanced,
		DefaultValue: f.Default,
		Fixed:        f.Fixed,
		Choices:      make([]*syncrv1.FieldChoice, 0, len(f.Choices)),
	}
	for _, c := range f.Choices {
		out.Choices = append(out.Choices, &syncrv1.FieldChoice{Value: c.Value, Label: c.Label})
	}
	return out
}

// fieldKindToProto converts a field kind; the backend-only members and
// wrapped kinds are FIELD_KIND_MEMBERS and FIELD_KIND_WRAPPED.
func fieldKindToProto(k descriptor.Kind) syncrv1.FieldKind {
	switch k {
	case descriptor.KindString:
		return syncrv1.FieldKind_FIELD_KIND_STRING
	case descriptor.KindText:
		return syncrv1.FieldKind_FIELD_KIND_TEXT
	case descriptor.KindInt:
		return syncrv1.FieldKind_FIELD_KIND_INT
	case descriptor.KindBool:
		return syncrv1.FieldKind_FIELD_KIND_BOOL
	case descriptor.KindEnum:
		return syncrv1.FieldKind_FIELD_KIND_ENUM
	case descriptor.Kind(backend.KindMembers):
		return syncrv1.FieldKind_FIELD_KIND_MEMBERS
	case descriptor.Kind(backend.KindWrapped):
		return syncrv1.FieldKind_FIELD_KIND_WRAPPED
	}
	return syncrv1.FieldKind_FIELD_KIND_UNSPECIFIED
}
