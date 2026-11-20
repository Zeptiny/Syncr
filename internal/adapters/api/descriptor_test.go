package api

import (
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/descriptor"
)

func TestFieldToProto(t *testing.T) {
	t.Parallel()
	f := descriptor.Field{
		Name: "security", Label: "Security", Help: "h", Placeholder: "p", Default: "starttls",
		Kind: descriptor.KindEnum, Required: true, Advanced: false,
		Choices: []descriptor.Choice{{Value: "starttls", Label: "STARTTLS"}, {Value: "tls", Label: "TLS"}},
	}
	got := fieldToProto(f)
	if got.GetName() != "security" || got.GetLabel() != "Security" || got.GetHelp() != "h" || got.GetPlaceholder() != "p" ||
		got.GetDefaultValue() != "starttls" || got.GetKind() != syncrv1.FieldKind_FIELD_KIND_ENUM || !got.GetRequired() ||
		len(got.GetChoices()) != 2 || got.GetChoices()[1].GetValue() != "tls" || got.GetChoices()[1].GetLabel() != "TLS" {
		t.Errorf("fieldToProto = %v", got)
	}
	if secret := fieldToProto(descriptor.Field{Name: "url", Kind: descriptor.KindString, Secret: true}); !secret.GetSecret() || secret.GetChoices() == nil || secret.GetFixed() {
		t.Errorf("secret field = %v (choices must be an empty list, not nil)", secret)
	}
	if fixed := fieldToProto(descriptor.Field{Name: "password", Kind: descriptor.KindString, Secret: true, Fixed: true}); !fixed.GetFixed() {
		t.Errorf("fixed field = %v", fixed)
	}
	for kind, want := range map[descriptor.Kind]syncrv1.FieldKind{
		descriptor.KindString:                syncrv1.FieldKind_FIELD_KIND_STRING,
		descriptor.KindText:                  syncrv1.FieldKind_FIELD_KIND_TEXT,
		descriptor.KindInt:                   syncrv1.FieldKind_FIELD_KIND_INT,
		descriptor.KindBool:                  syncrv1.FieldKind_FIELD_KIND_BOOL,
		descriptor.KindEnum:                  syncrv1.FieldKind_FIELD_KIND_ENUM,
		descriptor.Kind(backend.KindMembers): syncrv1.FieldKind_FIELD_KIND_MEMBERS,
		descriptor.Kind(backend.KindWrapped): syncrv1.FieldKind_FIELD_KIND_WRAPPED,
		"float":                              syncrv1.FieldKind_FIELD_KIND_UNSPECIFIED,
	} {
		if got := fieldKindToProto(kind); got != want {
			t.Errorf("fieldKindToProto(%q) = %v, want %v", kind, got, want)
		}
	}
	// A backend field keeps its members kind through Descriptor.
	members := backend.Field{Name: "members", Label: "Members", Kind: backend.KindMembers}
	if got := fieldToProto(members.Descriptor()); got.GetKind() != syncrv1.FieldKind_FIELD_KIND_MEMBERS {
		t.Errorf("members field = %v", got)
	}
	// And a wrapped field its wrapped kind, a fixed field its flag.
	wrapped := backend.Field{Name: "remote", Label: "Store in", Kind: backend.KindWrapped, Required: true}
	if got := fieldToProto(wrapped.Descriptor()); got.GetKind() != syncrv1.FieldKind_FIELD_KIND_WRAPPED || !got.GetRequired() || got.GetFixed() {
		t.Errorf("wrapped field = %v", got)
	}
	fixed := backend.Field{Name: "suffix", Label: "Suffix", Kind: backend.KindString, Default: ".bin", Advanced: true, Fixed: true}
	if got := fieldToProto(fixed.Descriptor()); !got.GetFixed() || !got.GetAdvanced() || got.GetDefaultValue() != ".bin" {
		t.Errorf("fixed field = %v", got)
	}
}

// TestBackendToProto: every capability reaches the wire form, and the
// fields keep their order, kinds and flags.
func TestBackendToProto(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		caps backend.Capabilities
		want *syncrv1.BackendCapabilities
	}{
		{backend.Capabilities{}, &syncrv1.BackendCapabilities{}},
		{backend.Capabilities{Local: true, UnionMember: true, ResticRepository: true},
			&syncrv1.BackendCapabilities{Local: true, UnionMember: true, ResticRepository: true}},
		{backend.Capabilities{Union: true}, &syncrv1.BackendCapabilities{Union: true}},
		{backend.Capabilities{Wrapper: true, NoChecksum: true}, &syncrv1.BackendCapabilities{Wrapper: true, NoChecksum: true}},
		// ObjectStore is not part of the wire form.
		{backend.Capabilities{ObjectStore: true, UnionMember: true}, &syncrv1.BackendCapabilities{UnionMember: true}},
	} {
		typ := capsBackend{caps: tt.caps}
		got := backendToProto(typ)
		if !proto.Equal(got.GetCapabilities(), tt.want) {
			t.Errorf("capabilities %+v = %v, want %v", tt.caps, got.GetCapabilities(), tt.want)
		}
		if got.GetId() != "wrap" || got.GetLabel() != "Wrap" || got.GetDescription() != "d" || len(got.GetFields()) != 2 ||
			got.GetFields()[0].GetKind() != syncrv1.FieldKind_FIELD_KIND_WRAPPED || !got.GetFields()[1].GetFixed() {
			t.Errorf("backend = %v", got)
		}
	}
}

// capsBackend is a backend type with the given capabilities, a wrapped
// field and a fixed secret, for TestBackendToProto (only its descriptor
// methods are used).
type capsBackend struct {
	backend.Type
	caps backend.Capabilities
}

func (capsBackend) ID() string                           { return "wrap" }
func (capsBackend) Label() string                        { return "Wrap" }
func (capsBackend) Description() string                  { return "d" }
func (c capsBackend) Capabilities() backend.Capabilities { return c.caps }

func (capsBackend) Fields() []backend.Field {
	return []backend.Field{
		{Name: "remote", Label: "Store in", Kind: backend.KindWrapped, Required: true},
		{Name: "password", Label: "Password", Kind: backend.KindString, Secret: true, Required: true, Fixed: true},
	}
}
