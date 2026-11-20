package crypt_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/crypt"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, crypt.Type{})
}

func TestDescriptor(t *testing.T) {
	t.Parallel()
	typ := crypt.Type{}
	if c := typ.Capabilities(); c != (backend.Capabilities{Wrapper: true, NoChecksum: true}) {
		t.Errorf("Capabilities = %+v, want a wrapper without checksums only", c)
	}
	fixed, wrapped := []string{}, []string{}
	for _, f := range typ.Fields() {
		if f.Fixed {
			fixed = append(fixed, f.Name)
		}
		if f.Kind == backend.KindWrapped {
			wrapped = append(wrapped, f.Name)
		}
		if f.Name == "password" && (!f.Secret || !f.Obscure || !f.Required || f.Help != crypt.PasswordHelp) {
			t.Errorf("password = %+v", f)
		}
	}
	want := []string{"password", "password2", "filename_encryption", "directory_name_encryption", "filename_encoding", "suffix"}
	if !slices.Equal(fixed, want) {
		t.Errorf("fixed fields = %q, want %q", fixed, want)
	}
	if !slices.Equal(wrapped, []string{"remote"}) {
		t.Errorf("wrapped fields = %q", wrapped)
	}
}

func TestValidateSuffix(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		suffix string
		ok     bool
	}{
		{"", true}, // the default, .bin
		{"none", true},
		{".bin", true},
		{".enc", true},
		{".é", true},
		{"." + strings.Repeat("x", 31), true},
		{"." + strings.Repeat("x", 32), false},
		{"bin", false},
		{".", false},
		{"..", true},
		{".a/b", false},
		{`.a\b`, false},
		{".a:b", false},
		{".a b", false},
		{".a b", false},
		{".a\x7fb", false},
		{"None", false},
		{".\xff", false},
	} {
		cfg := backend.Config{Options: map[string]string{"suffix": tt.suffix}, Secrets: map[string]string{"password": "pw"}}
		err := crypt.Type{}.Validate(cfg)
		if (err == nil) != tt.ok {
			t.Errorf("Validate(suffix %q) = %v, want ok %v", tt.suffix, err, tt.ok)
		}
	}
}

func TestToRclone(t *testing.T) {
	t.Parallel()
	r, err := crypt.Type{}.ToRclone(backend.Config{Secrets: map[string]string{"password": "pw", "password2": "salt"}})
	wantOpts := map[string]string{
		"filename_encryption": "standard", "directory_name_encryption": "true", "filename_encoding": "base32", "suffix": ".bin",
	}
	if err != nil || r.Backend != "crypt" || r.Root != "" || !maps.Equal(r.Options, wantOpts) ||
		!maps.Equal(r.Secrets, map[string]string{"password": "pw", "password2": "salt"}) ||
		!slices.Equal(r.ObscureKeys, []string{"password", "password2"}) {
		t.Errorf("ToRclone(defaults) = %+v, %v", r, err)
	}
	// The worker sets these; the descriptor never emits them, whatever the
	// configuration holds.
	for _, k := range []string{"remote", "strict_names", "pass_bad_blocks"} {
		if _, ok := r.Options[k]; ok {
			t.Errorf("ToRclone emits %q", k)
		}
	}
	bad := backend.Config{Options: map[string]string{"remote": "other:x", "strict_names": "false"}, Secrets: map[string]string{"password": "pw"}}
	if err := (crypt.Type{}).Validate(bad); err == nil {
		t.Error("Validate accepts remote and strict_names as options")
	}
	r, _ = crypt.Type{}.ToRclone(bad)
	if _, ok := r.Options["remote"]; ok {
		t.Error("ToRclone maps a remote option")
	}
	if _, ok := r.Options["strict_names"]; ok {
		t.Error("ToRclone maps strict_names")
	}
}
