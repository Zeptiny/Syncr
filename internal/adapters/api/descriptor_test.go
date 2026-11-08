package api

import (
	"testing"

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
	if secret := fieldToProto(descriptor.Field{Name: "url", Kind: descriptor.KindString, Secret: true}); !secret.GetSecret() || secret.GetChoices() == nil {
		t.Errorf("secret field = %v (choices must be an empty list, not nil)", secret)
	}
	for kind, want := range map[descriptor.Kind]syncrv1.FieldKind{
		descriptor.KindString:                syncrv1.FieldKind_FIELD_KIND_STRING,
		descriptor.KindText:                  syncrv1.FieldKind_FIELD_KIND_TEXT,
		descriptor.KindInt:                   syncrv1.FieldKind_FIELD_KIND_INT,
		descriptor.KindBool:                  syncrv1.FieldKind_FIELD_KIND_BOOL,
		descriptor.KindEnum:                  syncrv1.FieldKind_FIELD_KIND_ENUM,
		descriptor.Kind(backend.KindMembers): syncrv1.FieldKind_FIELD_KIND_MEMBERS,
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
}
