package backend_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// testFields is a descriptor's field list exercising every kind and flag.
func testFields() []backend.Field {
	return []backend.Field{
		{Name: "host", Label: "Host", Kind: backend.KindString, Required: true},
		{Name: "port", Label: "Port", Kind: backend.KindInt, Default: "22"},
		{Name: "user", Label: "User", Kind: backend.KindString, Rclone: "username"},
		{Name: "pass", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true},
		{Name: "key_pem", Label: "Key", Kind: backend.KindText, Secret: true, Rclone: "key_pem"},
		{Name: "token", Label: "Token", Kind: backend.KindString, Secret: true, Required: true},
		{Name: "insecure", Label: "Insecure", Kind: backend.KindBool, Advanced: true},
		{Name: "shell", Label: "Shell", Kind: backend.KindEnum, Advanced: true, Default: "unix",
			Choices: []backend.Choice{{Value: "unix", Label: "Unix"}, {Value: "cmd", Label: "Windows cmd"}}},
		{Name: "path", Label: "Path", Kind: backend.KindString, Rclone: backend.NotRclone},
		{Name: "members", Label: "Members", Kind: backend.KindMembers},
	}
}

func TestKindAndModeValid(t *testing.T) {
	t.Parallel()
	for _, k := range []backend.Kind{backend.KindString, backend.KindText, backend.KindInt, backend.KindBool, backend.KindEnum, backend.KindMembers} {
		if !k.Valid() {
			t.Errorf("%q is not valid", k)
		}
	}
	if backend.Kind("float").Valid() || backend.Kind("").Valid() {
		t.Error("unknown kind is valid")
	}
	for _, m := range []backend.UnionMode{backend.UnionReadWrite, backend.UnionReadOnly, backend.UnionNoCreate} {
		if !m.Valid() {
			t.Errorf("%q is not valid", m)
		}
	}
	if backend.UnionMode("ro").Valid() || backend.UnionMode("").Valid() {
		t.Error("unknown union mode is valid")
	}
}

func TestRcloneName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		f    backend.Field
		want string
	}{
		{backend.Field{Name: "host"}, "host"},
		{backend.Field{Name: "user", Rclone: "username"}, "username"},
		{backend.Field{Name: "bucket", Rclone: backend.NotRclone}, ""},
		{backend.Field{Name: "members", Kind: backend.KindMembers}, ""},
		{backend.Field{Name: "members", Kind: backend.KindMembers, Rclone: "upstreams"}, ""},
	} {
		if got := tt.f.RcloneName(); got != tt.want {
			t.Errorf("RcloneName(%+v) = %q, want %q", tt.f, got, tt.want)
		}
	}
}

func TestClonesDoNotAlias(t *testing.T) {
	t.Parallel()
	c := backend.Config{Options: map[string]string{"a": "1"}, Secrets: map[string]string{"s": "x"}}
	cc := c.Clone()
	cc.Options["a"], cc.Secrets["s"] = "2", "y"
	if c.Options["a"] != "1" || c.Secrets["s"] != "x" {
		t.Error("Config.Clone aliases its maps")
	}
	if z := (backend.Config{}).Clone(); z.Options != nil || z.Secrets != nil {
		t.Error("Clone of the zero Config is not zero")
	}

	r := backend.Rclone{Backend: "sftp", Options: map[string]string{"a": "1"}, Secrets: map[string]string{"s": "x"}, ObscureKeys: []string{"s"}, Root: "/r"}
	rc := r.Clone()
	rc.Options["a"], rc.Secrets["s"], rc.ObscureKeys[0] = "2", "y", "z"
	if r.Options["a"] != "1" || r.Secrets["s"] != "x" || r.ObscureKeys[0] != "s" || rc.Root != "/r" || rc.Backend != "sftp" {
		t.Error("Rclone.Clone aliases or loses fields")
	}
}

func fieldPaths(errs []domain.FieldError) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Field)
	}
	return out
}

func TestCheckFields(t *testing.T) {
	t.Parallel()
	valid := backend.Config{
		Options: map[string]string{"host": "example.org", "port": "2222", "insecure": "false", "shell": "cmd", "path": "/srv"},
		Secrets: map[string]string{"pass": "hunter2", "token": "t", "key_pem": "-----BEGIN\nKEY\n-----END\n"},
	}
	if errs := backend.CheckFields(testFields(), valid); len(errs) != 0 {
		t.Fatalf("valid config: %v", errs)
	}
	minimal := backend.Config{Options: map[string]string{"host": "h"}, Secrets: map[string]string{"token": "t"}}
	if errs := backend.CheckFields(testFields(), minimal); len(errs) != 0 {
		t.Fatalf("minimal config: %v", errs)
	}
	// Empty optional values are fine; nil maps are fine.
	if errs := backend.CheckFields(testFields(), backend.Config{Options: map[string]string{"host": "h", "port": ""}, Secrets: map[string]string{"token": "t", "pass": ""}}); len(errs) != 0 {
		t.Fatalf("empty optional values: %v", errs)
	}

	tests := []struct {
		name string
		cfg  backend.Config
		want []string
	}{
		{"nothing", backend.Config{}, []string{"options.host", "secrets.token"}},
		{"empty required", backend.Config{Options: map[string]string{"host": ""}, Secrets: map[string]string{"token": ""}}, []string{"options.host", "secrets.token"}},
		{"unknown option and secret", backend.Config{
			Options: map[string]string{"host": "h", "zzz": "1", "aaa": "2"},
			Secrets: map[string]string{"token": "t", "nope": "x"},
		}, []string{"options.aaa", "options.zzz", "secrets.nope"}},
		{"secret in options", backend.Config{Options: map[string]string{"host": "h", "pass": "x", "token": "t"}}, []string{"secrets.token", "options.pass", "options.token"}},
		{"option in secrets", backend.Config{Options: map[string]string{}, Secrets: map[string]string{"host": "h", "token": "t"}}, []string{"options.host", "secrets.host"}},
		{"members are not config", backend.Config{Options: map[string]string{"host": "h", "members": "a,b"}, Secrets: map[string]string{"token": "t"}}, []string{"options.members"}},
		{"bad int", cfgWith("port", "22a"), []string{"options.port"}},
		{"int with space", cfgWith("port", " 22"), []string{"options.port"}},
		{"bad bool", cfgWith("insecure", "yes"), []string{"options.insecure"}},
		{"bool is case sensitive", cfgWith("insecure", "True"), []string{"options.insecure"}},
		{"bad enum", cfgWith("shell", "bash"), []string{"options.shell"}},
		{"multi-line string", cfgWith("user", "a\nb"), []string{"options.user"}},
		{"NUL", cfgWith("path", "a\x00"), []string{"options.path"}},
		{"NUL in secret", backend.Config{Options: map[string]string{"host": "h"}, Secrets: map[string]string{"token": "t\x00"}}, []string{"secrets.token"}},
	}
	for _, tt := range tests {
		errs := backend.CheckFields(testFields(), tt.cfg)
		if got := fieldPaths(errs); !slices.Equal(got, tt.want) {
			t.Errorf("%s: errors on %q, want %q (%v)", tt.name, got, tt.want, errs)
		}
		for _, e := range errs {
			if e.Message == "" {
				t.Errorf("%s: empty message for %s", tt.name, e.Field)
			}
		}
	}
}

// cfgWith is a valid minimal config with one more option.
func cfgWith(key, value string) backend.Config {
	return backend.Config{Options: map[string]string{"host": "h", key: value}, Secrets: map[string]string{"token": "t"}}
}

func TestCheckFieldsNeverEchoesValues(t *testing.T) {
	t.Parallel()
	const sentinel = "SENTINEL-VALUE-1234"
	cfg := backend.Config{
		Options: map[string]string{"host": sentinel + "\n", "port": sentinel, "pass": sentinel, sentinel: sentinel},
		Secrets: map[string]string{"token": sentinel + "\x00", "host": sentinel},
	}
	for _, e := range backend.CheckFields(testFields(), cfg) {
		if strings.Contains(e.Message, sentinel) {
			t.Errorf("message of %s echoes the value: %q", e.Field, e.Message)
		}
	}
}

func TestMapFields(t *testing.T) {
	t.Parallel()
	cfg := backend.Config{
		Options: map[string]string{"host": "example.org", "user": "bob", "insecure": "", "path": "/srv", "unknown": "x"},
		Secrets: map[string]string{"pass": "hunter2", "token": "t", "key_pem": "", "stray": "y"},
	}
	got := backend.MapFields(testFields(), cfg)
	wantOpts := map[string]string{"host": "example.org", "port": "22", "username": "bob", "shell": "unix"}
	wantSecrets := map[string]string{"pass": "hunter2", "token": "t"}
	if !maps.Equal(got.Options, wantOpts) {
		t.Errorf("Options = %v, want %v", got.Options, wantOpts)
	}
	if !maps.Equal(got.Secrets, wantSecrets) {
		t.Errorf("Secrets = %v, want %v", got.Secrets, wantSecrets)
	}
	if !slices.Equal(got.ObscureKeys, []string{"pass"}) {
		t.Errorf("ObscureKeys = %v", got.ObscureKeys)
	}
	if got.Backend != "" || got.Root != "" {
		t.Errorf("Backend/Root set: %+v", got)
	}

	empty := backend.MapFields(nil, backend.Config{})
	if empty.Options == nil || empty.Secrets == nil || len(empty.ObscureKeys) != 0 {
		t.Errorf("MapFields(nil) = %+v, want empty non-nil maps", empty)
	}
	// An obscured secret left empty is not listed.
	noPass := backend.MapFields(testFields(), backend.Config{Options: map[string]string{"host": "h"}, Secrets: map[string]string{"token": "t"}})
	if len(noPass.ObscureKeys) != 0 {
		t.Errorf("ObscureKeys = %v, want none", noPass.ObscureKeys)
	}
	// Deterministic.
	again := backend.MapFields(testFields(), cfg)
	if !maps.Equal(again.Options, got.Options) || !maps.Equal(again.Secrets, got.Secrets) || !slices.Equal(again.ObscureKeys, got.ObscureKeys) {
		t.Error("MapFields is not deterministic")
	}
	// The result does not alias the input.
	got.Options["host"] = "changed"
	if cfg.Options["host"] != "example.org" {
		t.Error("MapFields aliases the config")
	}
}

func TestJoinRemote(t *testing.T) {
	t.Parallel()
	ok := []struct{ root, rel, want string }{
		{"", "", ""},
		{"bucket", "", "bucket"},
		{"bucket", "a/b", "bucket/a/b"},
		{"bucket/", "/a/b/", "bucket/a/b"},
		{"bucket/prefix", "./a//b/.", "bucket/prefix/a/b"},
		{"", "a/b", "a/b"},
		{"", "/abs/dir", "/abs/dir"},
		{"/home/user", "/etc", "/home/user/etc"},
		{"/", "", "/"},
		{"/", "x", "/x"},
		{"bucket", "..a/b..", "bucket/..a/b.."},
		{"bucket", `a\..\b`, `bucket/a\..\b`}, // backslashes are ordinary characters on remotes
		{"bucket", "dir:ro", "bucket/dir:ro"},
	}
	for _, tt := range ok {
		got, err := backend.JoinRemote(tt.root, tt.rel)
		if err != nil || got != tt.want {
			t.Errorf("JoinRemote(%q, %q) = (%q, %v), want %q", tt.root, tt.rel, got, err, tt.want)
		}
	}
	bad := []struct{ root, rel string }{
		{"bucket", ".."},
		{"bucket", "a/../b"},
		{"bucket", "a/.."},
		{"bucket/..", "a"},
		{"bucket", "a\x00b"},
		{"bucket", "a\nb"},
		{"bucket\r", "a"},
		{"bucket", strings.Repeat("a", backend.MaxPathLen+1)},
		{strings.Repeat("a", backend.MaxPathLen-1), "bb"},
	}
	for _, tt := range bad {
		_, err := backend.JoinRemote(tt.root, tt.rel)
		var pe *backend.PathError
		if !errors.As(err, &pe) || !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("JoinRemote(%.20q, %.20q) err = %v, want a *PathError matching ErrInvalidArgument", tt.root, tt.rel, err)
		}
	}
}

func TestJoinLocal(t *testing.T) {
	t.Parallel()
	ok := []struct{ root, rel, want string }{
		{"/srv/photos", "", "/srv/photos"},
		{"/srv/photos", "2024/jan", "/srv/photos/2024/jan"},
		{"/srv/photos/", "/2024/", "/srv/photos/2024"},
		{"/", "x", "/x"},
		{"/", "", "/"},
		{"/srv", `a\b`, `/srv/a\b`}, // POSIX: a backslash is part of the name
		{"/srv", `..\b`, `/srv/..\b`},
		{`C:\Data`, "", `C:\Data`},
		{`C:\Data`, "photos/2024", `C:\Data\photos\2024`},
		{`c:/Data/`, `\photos\.\2024\`, `c:\Data\photos\2024`},
		{`D:\`, "", `D:\`},
		{`D:\`, "x", `D:\x`},
		{`\\nas\share`, "", `\\nas\share`},
		{`\\nas\share\dir`, "a/b", `\\nas\share\dir\a\b`},
	}
	for _, tt := range ok {
		got, err := backend.JoinLocal(tt.root, tt.rel)
		if err != nil || got != tt.want {
			t.Errorf("JoinLocal(%q, %q) = (%q, %v), want %q", tt.root, tt.rel, got, err, tt.want)
		}
	}
	bad := []struct{ root, rel string }{
		{"", "x"},
		{"relative", "x"},
		{"C:", "x"},
		{"C:relative", "x"},
		{`\\nas`, "x"},
		{"/srv", "../etc"},
		{"/srv/..", "x"},
		{`C:\Data`, `a\..\..\Windows`},
		{`C:\Data`, "a/../b"},
		{`C:\Data`, "a\x00"},
		{"/srv", "a\r\nb"},
		{"/srv", strings.Repeat("a", backend.MaxPathLen)},
	}
	for _, tt := range bad {
		_, err := backend.JoinLocal(tt.root, tt.rel)
		var pe *backend.PathError
		if !errors.As(err, &pe) || !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("JoinLocal(%.20q, %.20q) err = %v, want a *PathError", tt.root, tt.rel, err)
		}
	}
}
