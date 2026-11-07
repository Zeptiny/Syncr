package sftp_test

import (
	"slices"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/sftp"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, sftp.Type{})
}

func TestValidateAndMap(t *testing.T) {
	t.Parallel()
	base := map[string]string{"host": "h", "user": "u"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{"host": "h", "user": "u"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tt := range []struct {
		name    string
		opts    map[string]string
		secrets map[string]string
		wantErr bool
	}{
		{"password", base, map[string]string{"pass": "p"}, false},
		{"key", base, map[string]string{"key_pem": "k"}, false},
		{"neither password nor key", base, map[string]string{"key_file_pass": "x"}, true},
		{"port out of range", with("port", "70000"), map[string]string{"pass": "p"}, true},
		{"port zero", with("port", "0"), map[string]string{"pass": "p"}, true},
		{"dot-dot path", with("path", "/srv/../etc"), map[string]string{"pass": "p"}, true},
	} {
		if err := (sftp.Type{}).Validate(backend.Config{Options: tt.opts, Secrets: tt.secrets}); (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate = %v, want error %v", tt.name, err, tt.wantErr)
		}
	}

	r, err := sftp.Type{}.ToRclone(backend.Config{Options: with("path", "/srv//data/"), Secrets: map[string]string{"pass": "p", "key_file_pass": "k"}})
	if err != nil || r.Root != "/srv/data" || r.Options["port"] != "22" || !slices.Equal(r.ObscureKeys, []string{"pass", "key_file_pass"}) {
		t.Errorf("ToRclone = %+v, %v", r, err)
	}
}
