package s3_test

import (
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/s3"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, s3.Type{})
}

func TestValidateAndMap(t *testing.T) {
	t.Parallel()
	secret := map[string]string{"secret_access_key": "s"}
	for _, tt := range []struct {
		name    string
		opts    map[string]string
		wantErr bool
		root    string
	}{
		{"AWS needs no endpoint", map[string]string{"access_key_id": "a"}, false, ""},
		{"other providers need one", map[string]string{"provider": "Minio", "access_key_id": "a"}, true, ""},
		{"bucket and prefix", map[string]string{"access_key_id": "a", "bucket": "/b/prefix/"}, false, "b/prefix"},
		{"no dot-dot in the bucket", map[string]string{"access_key_id": "a", "bucket": "b/../c"}, true, ""},
	} {
		cfg := backend.Config{Options: tt.opts, Secrets: secret}
		if err := (s3.Type{}).Validate(cfg); (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate = %v, want error %v", tt.name, err, tt.wantErr)
		}
		if tt.wantErr {
			continue
		}
		r, err := s3.Type{}.ToRclone(cfg)
		if err != nil || r.Root != tt.root || r.Options["provider"] != "AWS" || r.Secrets["secret_access_key"] != "s" {
			t.Errorf("%s: ToRclone = %+v, %v", tt.name, r, err)
		}
	}
}
