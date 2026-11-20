package smb_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/smb"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, smb.Type{})
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	want := backend.Capabilities{UnionMember: true, ResticRepository: true, NoChecksum: true}
	if got := (smb.Type{}).Capabilities(); got != want {
		t.Errorf("Capabilities = %+v, want %+v", got, want)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	const secret = "smb-secret-value"
	for _, tt := range []struct {
		name    string
		opts    map[string]string
		secrets map[string]string
		field   string // "" for valid
	}{
		{"guest without a password", map[string]string{"host": "nas.local", "user": "guest"}, nil, ""},
		{"user and password", map[string]string{"host": "nas.local", "user": "backup", "share": "data/sub"}, map[string]string{"pass": secret}, ""},
		{"no user: never rclone's default", map[string]string{"host": "nas.local"}, nil, "options.user"},
		{"no host", map[string]string{"user": "backup"}, nil, "options.host"},
		{"port too big", map[string]string{"host": "h", "user": "u", "port": "65536"}, nil, "options.port"},
		{"port not a number", map[string]string{"host": "h", "user": "u", "port": "smb"}, nil, "options.port"},
		{"dot-dot share", map[string]string{"host": "h", "user": "u", "share": "../c$"}, nil, "options.share"},
		{"line break in the password", map[string]string{"host": "h", "user": "u"}, map[string]string{"pass": secret + "\r\nx"}, "secrets.pass"},
	} {
		err := smb.Type{}.Validate(backend.Config{Options: tt.opts, Secrets: tt.secrets})
		if tt.field == "" {
			if err != nil {
				t.Errorf("%s: Validate = %v", tt.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.field) {
			t.Errorf("%s: Validate = %v, want an error on %s", tt.name, err, tt.field)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error contains the password", tt.name)
		}
	}
}

func TestToRclone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cfg  backend.Config
		want backend.Rclone
	}{
		{"defaults", backend.Config{Options: map[string]string{"host": "nas.local", "user": "guest"}},
			backend.Rclone{Backend: "smb", Options: map[string]string{"host": "nas.local", "user": "guest", "domain": "WORKGROUP", "port": "445"},
				Secrets: map[string]string{}}},
		{"everything", backend.Config{
			Options: map[string]string{"host": "fs", "user": "u", "share": "/backups//syncr/", "domain": "CORP", "port": "4450",
				"spn": "cifs/fs", "case_insensitive": "false"},
			Secrets: map[string]string{"pass": "p"},
		}, backend.Rclone{Backend: "smb", Root: "backups/syncr",
			Options: map[string]string{"host": "fs", "user": "u", "domain": "CORP", "port": "4450", "spn": "cifs/fs", "case_insensitive": "false"},
			Secrets: map[string]string{"pass": "p"}, ObscureKeys: []string{"pass"}}},
	} {
		got, err := smb.Type{}.ToRclone(tt.cfg)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: ToRclone = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
}
