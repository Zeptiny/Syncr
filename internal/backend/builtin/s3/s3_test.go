package s3_test

import (
	"reflect"
	"slices"
	"strings"
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

// TestProviders: the providers in form order, rclone's spellings (checked
// against the pinned rclone by the worker's cross-check test), with
// "Other" last.
func TestProviders(t *testing.T) {
	t.Parallel()
	var got []string
	for _, f := range (s3.Type{}).Fields() {
		if f.Name == "provider" {
			for _, c := range f.Choices {
				got = append(got, c.Value)
			}
		}
	}
	want := []string{
		"AWS", "Minio", "Cloudflare", "Wasabi", "DigitalOcean", "Ceph",
		"Linode", "Alibaba", "Hetzner", "IBMCOS", "IDrive", "IONOS", "Mega", "OVHcloud", "Scaleway", "SeaweedFS", "Storj", "Synology",
		"Other",
	}
	if !slices.Equal(got, want) {
		t.Errorf("providers = %q, want %q", got, want)
	}
	// Every provider but AWS needs an endpoint: rclone 1.75.2 sends the
	// requests of any of them to AWS without one.
	for _, p := range want[1:] {
		cfg := backend.Config{Options: map[string]string{"provider": p, "access_key_id": "a"}, Secrets: map[string]string{"secret_access_key": "s"}}
		if err := (s3.Type{}).Validate(cfg); err == nil || !strings.Contains(err.Error(), "options.endpoint") {
			t.Errorf("provider %s without an endpoint: Validate = %v", p, err)
		}
		cfg.Options["endpoint"] = "https://s3.example.com"
		if err := (s3.Type{}).Validate(cfg); err != nil {
			t.Errorf("provider %s with an endpoint: Validate = %v", p, err)
		}
	}
}

// TestEmittedOptionsUnchanged pins what existing s3 remotes emit: adding
// providers changed no mapping.
func TestEmittedOptionsUnchanged(t *testing.T) {
	t.Parallel()
	exs := s3.Type{}.Examples()
	want := []backend.Rclone{
		{Backend: "s3", Root: "backups",
			Options: map[string]string{"provider": "AWS", "access_key_id": "AKIAEXAMPLE", "region": "eu-west-1"},
			Secrets: map[string]string{"secret_access_key": "aws-secret"}},
		{Backend: "s3", Root: "archive/photos",
			Options: map[string]string{
				"provider": "Minio", "access_key_id": "minio", "region": "us-east-1", "endpoint": "https://minio.example.com:9000",
				"location_constraint": "us-east-1", "acl": "private", "storage_class": "STANDARD",
				"server_side_encryption": "aws:kms", "sse_kms_key_id": "key-1", "force_path_style": "true", "no_check_bucket": "true",
			},
			Secrets: map[string]string{"secret_access_key": "minio-secret"}},
	}
	if len(exs) != len(want) {
		t.Fatalf("%d examples, want %d", len(exs), len(want))
	}
	for i, cfg := range exs {
		got, err := s3.Type{}.ToRclone(cfg)
		if err != nil || !reflect.DeepEqual(got, want[i]) {
			t.Errorf("example %d: ToRclone = %+v, %v; want %+v", i, got, err, want[i])
		}
	}
	for _, p := range []string{"Minio", "Cloudflare", "Wasabi", "DigitalOcean", "Ceph", "Other"} {
		got, err := s3.Type{}.ToRclone(backend.Config{
			Options: map[string]string{"provider": p, "access_key_id": "a", "endpoint": "https://e"},
			Secrets: map[string]string{"secret_access_key": "s"},
		})
		w := backend.Rclone{Backend: "s3", Options: map[string]string{"provider": p, "access_key_id": "a", "endpoint": "https://e"},
			Secrets: map[string]string{"secret_access_key": "s"}}
		if err != nil || !reflect.DeepEqual(got, w) {
			t.Errorf("provider %s: ToRclone = %+v, %v", p, got, err)
		}
	}
}
