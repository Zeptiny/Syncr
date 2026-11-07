// Package s3 is the backend descriptor of Amazon S3 and S3-compatible
// object stores (rclone backend "s3").
package s3

import (
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the s3 backend type.
type Type struct{}

var _ backend.Exampler = Type{}

func choices(values ...string) []backend.Choice {
	out := make([]backend.Choice, len(values))
	for i, v := range values {
		out[i] = backend.Choice{Value: v, Label: v}
	}
	return out
}

// ID implements backend.Type.
func (Type) ID() string { return "s3" }

// Label implements backend.Type.
func (Type) Label() string { return "Amazon S3 and compatible" }

// Description implements backend.Type.
func (Type) Description() string {
	return "Object storage that speaks the S3 API: AWS, MinIO, Cloudflare R2, Wasabi, DigitalOcean Spaces, Ceph and others."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "provider", Label: "Provider", Kind: backend.KindEnum, Default: "AWS", Choices: []backend.Choice{
			{Value: "AWS", Label: "Amazon Web Services"}, {Value: "Minio", Label: "MinIO"},
			{Value: "Cloudflare", Label: "Cloudflare R2"}, {Value: "Wasabi", Label: "Wasabi"},
			{Value: "DigitalOcean", Label: "DigitalOcean Spaces"}, {Value: "Ceph", Label: "Ceph"},
			{Value: "Other", Label: "Other S3-compatible"},
		}},
		{Name: "access_key_id", Label: "Access key ID", Kind: backend.KindString, Required: true},
		{Name: "secret_access_key", Label: "Secret access key", Kind: backend.KindString, Required: true, Secret: true},
		{Name: "region", Label: "Region", Kind: backend.KindString, Placeholder: "us-east-1"},
		{Name: "endpoint", Label: "Endpoint", Kind: backend.KindString, Placeholder: "https://s3.example.com",
			Help: "Required unless the provider is AWS."},
		{Name: "bucket", Label: "Bucket", Kind: backend.KindString, Rclone: backend.NotRclone,
			Help: "Bucket, optionally with a path prefix (bucket/prefix). Leave empty to name the bucket in each job's path."},
		{Name: "location_constraint", Label: "Location constraint", Kind: backend.KindString, Advanced: true},
		{Name: "acl", Label: "Canned ACL", Kind: backend.KindEnum, Advanced: true, Choices: choices(
			"private", "public-read", "public-read-write", "authenticated-read", "bucket-owner-read", "bucket-owner-full-control")},
		{Name: "storage_class", Label: "Storage class", Kind: backend.KindString, Advanced: true, Placeholder: "STANDARD"},
		{Name: "server_side_encryption", Label: "Server-side encryption", Kind: backend.KindEnum, Advanced: true,
			Choices: []backend.Choice{{Value: "AES256", Label: "AES256"}, {Value: "aws:kms", Label: "AWS KMS"}}},
		{Name: "sse_kms_key_id", Label: "KMS key ID", Kind: backend.KindString, Advanced: true},
		{Name: "force_path_style", Label: "Path-style URLs", Kind: backend.KindBool, Advanced: true},
		{Name: "no_check_bucket", Label: "Don't check or create the bucket", Kind: backend.KindBool, Advanced: true},
	}
}

// Capabilities implements backend.Type.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true}
}

// Validate implements backend.Type: the field checks, an endpoint for
// providers other than AWS, and a well-formed bucket path.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if p := cfg.Options["provider"]; p != "" && p != "AWS" && cfg.Options["endpoint"] == "" {
		v.Add("options.endpoint", "is required for this provider")
	}
	if _, err := backend.JoinRemote(cfg.Options["bucket"], ""); err != nil {
		v.Add("options.bucket", err.Error())
	}
	return v.Err()
}

// ToRclone implements backend.Type; the bucket becomes the Root.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "s3"
	r.Root = strings.Trim(cfg.Options["bucket"], "/")
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"provider": "AWS", "access_key_id": "AKIAEXAMPLE", "region": "eu-west-1", "bucket": "backups"},
			Secrets: map[string]string{"secret_access_key": "aws-secret"}},
		{
			Options: map[string]string{
				"provider": "Minio", "access_key_id": "minio", "region": "us-east-1", "endpoint": "https://minio.example.com:9000",
				"bucket": "archive/photos", "location_constraint": "us-east-1", "acl": "private", "storage_class": "STANDARD",
				"server_side_encryption": "aws:kms", "sse_kms_key_id": "key-1", "force_path_style": "true", "no_check_bucket": "true",
			},
			Secrets: map[string]string{"secret_access_key": "minio-secret"},
		},
	}
}
