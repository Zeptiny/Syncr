package ftp_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/ftp"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, ftp.Type{})
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	want := backend.Capabilities{UnionMember: true, ResticRepository: true, NoChecksum: true}
	if got := (ftp.Type{}).Capabilities(); got != want {
		t.Errorf("Capabilities = %+v, want %+v", got, want)
	}
}

// TestLineFieldsAreSingleLine pins what the line-break refusal relies on:
// host, user and pass are single-line string fields, which CheckFields
// refuses with a line break.
func TestLineFieldsAreSingleLine(t *testing.T) {
	t.Parallel()
	fields := ftp.Type{}.Fields()
	for _, name := range []string{"host", "user", "pass"} {
		i := slices.IndexFunc(fields, func(f backend.Field) bool { return f.Name == name })
		if i < 0 || fields[i].Kind != backend.KindString {
			t.Errorf("field %q is not a single-line string", name)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	const secret = "ftp-secret-value"
	base := func(kv ...string) map[string]string {
		m := map[string]string{"host": "ftp.example.com", "user": "backup"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tt := range []struct {
		name    string
		opts    map[string]string
		secrets map[string]string
		field   string // "" for valid
	}{
		{"anonymous", base("user", "anonymous"), nil, ""},
		{"password and explicit TLS", base("explicit_tls", "true"), map[string]string{"pass": secret}, ""},
		{"implicit TLS on 990", base("tls", "true", "port", "990"), nil, ""},
		{"TLS flags both false", base("tls", "false", "explicit_tls", "false"), nil, ""},
		{"no user: never rclone's default", map[string]string{"host": "h"}, nil, "options.user"},
		{"both TLS kinds", base("tls", "true", "explicit_tls", "true"), nil, "options.explicit_tls"},
		{"LF in host", base("host", "h\nQUIT"), nil, "options.host"},
		{"CR in host", base("host", "h\rQUIT"), nil, "options.host"},
		{"LF in user", base("user", "u\nQUIT"), nil, "options.user"},
		{"CR in user", base("user", "u\rQUIT"), nil, "options.user"},
		{"LF in pass", base(), map[string]string{"pass": secret + "\nDELE x"}, "secrets.pass"},
		{"CR in pass", base(), map[string]string{"pass": secret + "\rDELE x"}, "secrets.pass"},
		{"port zero", base("port", "0"), nil, "options.port"},
		{"negative concurrency", base("concurrency", "-2"), nil, "options.concurrency"},
		{"dot-dot path", base("path", "a/../../b"), nil, "options.path"},
	} {
		err := ftp.Type{}.Validate(backend.Config{Options: tt.opts, Secrets: tt.secrets})
		if tt.field == "" {
			if err != nil {
				t.Errorf("%s: Validate = %v", tt.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.field) {
			t.Errorf("%s: Validate = %v, want an error on %s", tt.name, err, tt.field)
		}
		if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "QUIT")) {
			t.Errorf("%s: the error contains a value: %v", tt.name, err)
		}
	}
}

// TestToRcloneAlwaysSendsPass: rclone 1.75.2 reveals pass unconditionally,
// so it is an obscured secret even when empty.
func TestToRcloneAlwaysSendsPass(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cfg  backend.Config
		want backend.Rclone
	}{
		{"anonymous: an empty obscured pass", backend.Config{Options: map[string]string{"host": "ftp.example.com", "user": "anonymous"}},
			backend.Rclone{Backend: "ftp", Options: map[string]string{"host": "ftp.example.com", "user": "anonymous", "port": "21"},
				Secrets: map[string]string{"pass": ""}, ObscureKeys: []string{"pass"}}},
		{"an empty pass in the config", backend.Config{Options: map[string]string{"host": "h", "user": "u"}, Secrets: map[string]string{"pass": ""}},
			backend.Rclone{Backend: "ftp", Options: map[string]string{"host": "h", "user": "u", "port": "21"},
				Secrets: map[string]string{"pass": ""}, ObscureKeys: []string{"pass"}}},
		{"everything", backend.Config{
			Options: map[string]string{"host": "h", "user": "u", "tls": "true", "path": "/srv//data/", "port": "990",
				"no_check_certificate": "true", "concurrency": "2", "disable_epsv": "true", "disable_mlsd": "false", "writing_mdtm": "true"},
			Secrets: map[string]string{"pass": "p"},
		}, backend.Rclone{Backend: "ftp", Root: "/srv/data",
			Options: map[string]string{"host": "h", "user": "u", "tls": "true", "port": "990", "no_check_certificate": "true",
				"concurrency": "2", "disable_epsv": "true", "disable_mlsd": "false", "writing_mdtm": "true"},
			Secrets: map[string]string{"pass": "p"}, ObscureKeys: []string{"pass"}}},
	} {
		got, err := ftp.Type{}.ToRclone(tt.cfg)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: ToRclone = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
}
