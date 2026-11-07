package backend_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// stubType is a configurable descriptor for registry tests.
type stubType struct {
	id, label string
	caps      backend.Capabilities
	fields    []backend.Field
}

func (s stubType) ID() string                         { return s.id }
func (s stubType) Label() string                      { return s.label }
func (s stubType) Description() string                { return "stub " + s.id }
func (s stubType) Fields() []backend.Field            { return slices.Clone(s.fields) }
func (s stubType) Capabilities() backend.Capabilities { return s.caps }

func (s stubType) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(s.fields, cfg))
	return v.Err()
}

func (s stubType) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(s.fields, cfg)
	r.Backend = s.id
	return r, nil
}

func stub(id, label string, fields ...backend.Field) stubType {
	return stubType{id: id, label: label, caps: backend.Capabilities{UnionMember: true}, fields: fields}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	b2 := stub("b2", "Backblaze B2")
	s3 := stub("s3", "Amazon S3")
	sftp := stub("sftp", "SFTP")
	alt := stub("abc", "SFTP") // same label: ordered by ID
	union := stubType{id: "union", label: "Union", caps: backend.Capabilities{Union: true},
		fields: []backend.Field{{Name: "members", Label: "Members", Kind: backend.KindMembers, Required: true}}}

	r, err := backend.NewRegistry(sftp, union, s3, b2, alt)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, typ := range r.Types() {
		ids = append(ids, typ.ID())
	}
	if want := []string{"s3", "b2", "abc", "sftp", "union"}; !slices.Equal(ids, want) {
		t.Errorf("Types() = %v, want %v (by label, then ID)", ids, want)
	}
	if got, ok := r.Get("s3"); !ok || got.ID() != "s3" {
		t.Errorf("Get(s3) = (%v, %v)", got, ok)
	}
	if _, ok := r.Get("ftp"); ok {
		t.Error("Get(ftp) found a type")
	}
	// Types returns a copy.
	ts := r.Types()
	ts[0] = nil
	if r.Types()[0] == nil {
		t.Error("Types() aliases the registry")
	}

	empty, err := backend.NewRegistry()
	if err != nil || len(empty.Types()) != 0 {
		t.Errorf("NewRegistry() = (%v, %v)", empty, err)
	}
}

func TestRegistryRejectsDuplicatesAndNil(t *testing.T) {
	t.Parallel()
	if _, err := backend.NewRegistry(stub("s3", "A"), stub("s3", "B")); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate IDs: err = %v", err)
	}
	if _, err := backend.NewRegistry(stub("s3", "A"), nil); err == nil {
		t.Error("nil type accepted")
	}
}

func TestCheckDescriptor(t *testing.T) {
	t.Parallel()
	str := func(name string) backend.Field {
		return backend.Field{Name: name, Label: name, Kind: backend.KindString}
	}
	enum := backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum, Choices: []backend.Choice{{Value: "a", Label: "A"}}}
	members := backend.Field{Name: "members", Label: "Members", Kind: backend.KindMembers}
	unionCaps := backend.Capabilities{Union: true}

	good := []stubType{
		stub("s3", "S3", str("host"), backend.Field{Name: "port", Label: "Port", Kind: backend.KindInt, Default: "22"},
			backend.Field{Name: "pass", Label: "Pass", Kind: backend.KindString, Secret: true, Obscure: true},
			backend.Field{Name: "bucket", Label: "Bucket", Kind: backend.KindString, Rclone: backend.NotRclone},
			backend.Field{Name: "path", Label: "Path", Kind: backend.KindString, Rclone: backend.NotRclone},
			backend.Field{Name: "flag", Label: "Flag", Kind: backend.KindBool, Default: "true", Advanced: true},
			backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum, Default: "a", Choices: enum.Choices},
			backend.Field{Name: "user_name", Label: "User", Kind: backend.KindString, Rclone: "user"}),
		{id: "union", label: "Union", caps: unionCaps, fields: []backend.Field{members}},
		{id: "local", label: "Local", caps: backend.Capabilities{Local: true, UnionMember: true, ResticRepository: true}},
	}
	for _, typ := range good {
		if err := backend.CheckDescriptor(typ); err != nil {
			t.Errorf("CheckDescriptor(%s): %v", typ.id, err)
		}
	}

	bad := map[string]stubType{
		"empty id":            stub("", "X"),
		"upper-case id":       stub("S3", "X"),
		"id with underscore":  stub("my_s3", "X"),
		"id starting digit":   stub("3s", "X"),
		"empty label":         stub("s3", ""),
		"union and local":     {id: "u", label: "U", caps: backend.Capabilities{Union: true, Local: true}, fields: []backend.Field{members}},
		"union and member":    {id: "u", label: "U", caps: backend.Capabilities{Union: true, UnionMember: true}, fields: []backend.Field{members}},
		"union w/o members":   {id: "u", label: "U", caps: unionCaps},
		"members, not union":  stub("s3", "S3", members),
		"two members":         {id: "u", label: "U", caps: unionCaps, fields: []backend.Field{members, {Name: "more", Label: "More", Kind: backend.KindMembers}}},
		"secret members":      {id: "u", label: "U", caps: unionCaps, fields: []backend.Field{{Name: "members", Label: "M", Kind: backend.KindMembers, Secret: true}}},
		"bad field name":      stub("s3", "S3", str("Host")),
		"dash in field name":  stub("s3", "S3", str("a-b")),
		"duplicate field":     stub("s3", "S3", str("host"), str("host")),
		"empty field label":   stub("s3", "S3", backend.Field{Name: "x", Kind: backend.KindString}),
		"unknown kind":        stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: "float"}),
		"obscure not secret":  stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindString, Obscure: true}),
		"required advanced":   stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindString, Required: true, Advanced: true}),
		"secret default":      stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindString, Secret: true, Default: "d"}),
		"bad rclone name":     stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindString, Rclone: "a b"}),
		"same rclone name":    stub("s3", "S3", str("user"), backend.Field{Name: "login", Label: "L", Kind: backend.KindString, Rclone: "user"}),
		"enum w/o choices":    stub("s3", "S3", backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum}),
		"choices on string":   stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindString, Choices: enum.Choices}),
		"empty choice value":  stub("s3", "S3", backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum, Choices: []backend.Choice{{Label: "A"}}}),
		"empty choice label":  stub("s3", "S3", backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum, Choices: []backend.Choice{{Value: "a"}}}),
		"duplicate choice":    stub("s3", "S3", backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum, Choices: []backend.Choice{{Value: "a", Label: "A"}, {Value: "a", Label: "B"}}}),
		"enum default":        stub("s3", "S3", backend.Field{Name: "e", Label: "E", Kind: backend.KindEnum, Default: "b", Choices: enum.Choices}),
		"int default":         stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindInt, Default: "ten"}),
		"bool default":        stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindBool, Default: "yes"}),
		"multi-line default":  stub("s3", "S3", backend.Field{Name: "x", Label: "X", Kind: backend.KindString, Default: "a\nb"}),
		"members has default": {id: "u", label: "U", caps: unionCaps, fields: []backend.Field{{Name: "members", Label: "M", Kind: backend.KindMembers, Default: "x"}}},
	}
	for name, typ := range bad {
		if err := backend.CheckDescriptor(typ); err == nil {
			t.Errorf("%s: CheckDescriptor accepted it", name)
		}
		if _, err := backend.NewRegistry(typ); err == nil {
			t.Errorf("%s: NewRegistry accepted it", name)
		}
	}
}
