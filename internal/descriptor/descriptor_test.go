package descriptor_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// testFields exercises every kind and flag.
func testFields() []descriptor.Field {
	return []descriptor.Field{
		{Name: "host", Label: "Host", Kind: descriptor.KindString, Required: true},
		{Name: "port", Label: "Port", Kind: descriptor.KindInt, Default: "587"},
		{Name: "password", Label: "Password", Kind: descriptor.KindString, Secret: true},
		{Name: "url", Label: "URL", Kind: descriptor.KindString, Secret: true, Required: true},
		{Name: "body", Label: "Body", Kind: descriptor.KindText},
		{Name: "tls", Label: "TLS", Kind: descriptor.KindBool, Advanced: true},
		{Name: "security", Label: "Security", Kind: descriptor.KindEnum, Default: "starttls",
			Choices: []descriptor.Choice{{Value: "starttls", Label: "STARTTLS"}, {Value: "tls", Label: "TLS"}}},
	}
}

func paths(errs []domain.FieldError) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Field)
	}
	return out
}

func TestKindValid(t *testing.T) {
	t.Parallel()
	for _, k := range []descriptor.Kind{descriptor.KindString, descriptor.KindText, descriptor.KindInt, descriptor.KindBool, descriptor.KindEnum} {
		if !k.Valid() {
			t.Errorf("%q is not valid", k)
		}
	}
	for _, k := range []descriptor.Kind{"", "members", "float"} {
		if k.Valid() {
			t.Errorf("%q is valid", k)
		}
	}
}

func TestValuesClone(t *testing.T) {
	t.Parallel()
	v := descriptor.Values{Options: map[string]string{"a": "1"}, Secrets: map[string]string{"s": "x"}}
	c := v.Clone()
	c.Options["a"], c.Secrets["s"] = "2", "y"
	if v.Options["a"] != "1" || v.Secrets["s"] != "x" {
		t.Error("Clone aliases its maps")
	}
	if z := (descriptor.Values{}).Clone(); z.Options != nil || z.Secrets != nil {
		t.Error("Clone of the zero Values is not zero")
	}
}

func TestCheckFields(t *testing.T) {
	t.Parallel()
	valid := descriptor.Values{
		Options: map[string]string{"host": "smtp.example", "port": "25", "body": "a\nb", "tls": "true", "security": "tls"},
		Secrets: map[string]string{"password": "pw", "url": "https://x"},
	}
	if errs := descriptor.CheckFields(testFields(), valid); len(errs) != 0 {
		t.Fatalf("valid: %v", errs)
	}
	with := func(key, value string) descriptor.Values {
		v := descriptor.Values{Options: map[string]string{"host": "h"}, Secrets: map[string]string{"url": "u"}}
		v.Options[key] = value
		return v
	}
	tests := []struct {
		name string
		v    descriptor.Values
		want []string
	}{
		{"nothing", descriptor.Values{}, []string{"options.host", "secrets.url"}},
		{"unknown keys", descriptor.Values{Options: map[string]string{"host": "h", "zz": "1", "aa": "1"}, Secrets: map[string]string{"url": "u", "no": "x"}},
			[]string{"options.aa", "options.zz", "secrets.no"}},
		{"secret in options", descriptor.Values{Options: map[string]string{"host": "h", "url": "u"}}, []string{"secrets.url", "options.url"}},
		{"option in secrets", descriptor.Values{Secrets: map[string]string{"host": "h", "url": "u"}}, []string{"options.host", "secrets.host"}},
		{"bad int", with("port", "x"), []string{"options.port"}},
		{"bad bool", with("tls", "yes"), []string{"options.tls"}},
		{"bad enum", with("security", "ssl"), []string{"options.security"}},
		{"multi-line string", with("host", "a\nb"), []string{"options.host"}},
		{"NUL in text", with("body", "a\x00"), []string{"options.body"}},
	}
	for _, tt := range tests {
		errs := descriptor.CheckFields(testFields(), tt.v)
		if got := paths(errs); !slices.Equal(got, tt.want) {
			t.Errorf("%s: errors on %q, want %q (%v)", tt.name, got, tt.want, errs)
		}
		for _, e := range errs {
			if e.Message == "" {
				t.Errorf("%s: empty message for %s", tt.name, e.Field)
			}
		}
	}
}

func TestCheckFieldsNeverEchoesValues(t *testing.T) {
	t.Parallel()
	const sentinel = "SENTINEL-9f2c"
	v := descriptor.Values{
		Options: map[string]string{"host": sentinel + "\n", "port": sentinel, "tls": sentinel, "security": sentinel, sentinel: "x", "password": sentinel},
		Secrets: map[string]string{"url": sentinel + "\x00", "host": sentinel},
	}
	errs := descriptor.CheckFields(testFields(), v)
	if len(errs) == 0 {
		t.Fatal("no errors")
	}
	for _, e := range errs {
		if strings.Contains(e.Message, sentinel) {
			t.Errorf("%s: message echoes the value: %q", e.Field, e.Message)
		}
	}
}

func TestCheckShape(t *testing.T) {
	t.Parallel()
	if err := descriptor.CheckShape(testFields()); err != nil {
		t.Fatalf("good shape: %v", err)
	}
	if err := descriptor.CheckShape(nil); err != nil {
		t.Fatalf("no fields: %v", err)
	}
	extra := descriptor.Field{Name: "members", Label: "Members", Kind: "members"}
	if err := descriptor.CheckShape([]descriptor.Field{extra}, "members"); err != nil {
		t.Errorf("extra kind: %v", err)
	}
	enum := []descriptor.Choice{{Value: "a", Label: "A"}}
	str := func(name string) descriptor.Field {
		return descriptor.Field{Name: name, Label: name, Kind: descriptor.KindString}
	}
	bad := map[string][]descriptor.Field{
		"bad name":           {str("Host")},
		"dash in name":       {str("a-b")},
		"duplicate name":     {str("a"), str("a")},
		"empty label":        {{Name: "x", Kind: descriptor.KindString}},
		"unknown kind":       {{Name: "x", Label: "X", Kind: "float"}},
		"extra kind not set": {extra},
		"required advanced":  {{Name: "x", Label: "X", Kind: descriptor.KindString, Required: true, Advanced: true}},
		"secret default":     {{Name: "x", Label: "X", Kind: descriptor.KindString, Secret: true, Default: "d"}},
		"enum w/o choices":   {{Name: "e", Label: "E", Kind: descriptor.KindEnum}},
		"choices on string":  {{Name: "x", Label: "X", Kind: descriptor.KindString, Choices: enum}},
		"empty choice":       {{Name: "e", Label: "E", Kind: descriptor.KindEnum, Choices: []descriptor.Choice{{Label: "A"}}}},
		"duplicate choice":   {{Name: "e", Label: "E", Kind: descriptor.KindEnum, Choices: []descriptor.Choice{{Value: "a", Label: "A"}, {Value: "a", Label: "B"}}}},
		"enum default":       {{Name: "e", Label: "E", Kind: descriptor.KindEnum, Default: "b", Choices: enum}},
		"int default":        {{Name: "x", Label: "X", Kind: descriptor.KindInt, Default: "ten"}},
		"bool default":       {{Name: "x", Label: "X", Kind: descriptor.KindBool, Default: "yes"}},
		"multi-line default": {{Name: "x", Label: "X", Kind: descriptor.KindString, Default: "a\nb"}},
	}
	for name, fields := range bad {
		err := descriptor.CheckShape(fields)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "field 0") && !strings.Contains(err.Error(), "field 1") {
			t.Errorf("%s: error does not name the field: %v", name, err)
		}
	}
}
