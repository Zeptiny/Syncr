// Package b2 is the backend descriptor of Backblaze B2 (rclone backend
// "b2").
package b2

import (
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the b2 backend type.
type Type struct{}

var _ backend.Exampler = Type{}

// ID implements backend.Type.
func (Type) ID() string { return "b2" }

// Label implements backend.Type.
func (Type) Label() string { return "Backblaze B2" }

// Description implements backend.Type.
func (Type) Description() string { return "Backblaze B2 cloud storage, with an application key." }

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "account", Label: "Key ID", Kind: backend.KindString, Required: true, Help: "The application key ID (or account ID)."},
		{Name: "key", Label: "Application key", Kind: backend.KindString, Required: true, Secret: true},
		{Name: "bucket", Label: "Bucket", Kind: backend.KindString, Rclone: backend.NotRclone,
			Help: "Bucket, optionally with a path prefix (bucket/prefix). Leave empty to name the bucket in each job's path."},
		{Name: "endpoint", Label: "Endpoint", Kind: backend.KindString, Advanced: true},
		{Name: "download_url", Label: "Download URL", Kind: backend.KindString, Advanced: true, Help: "e.g. a CDN in front of the bucket."},
		{Name: "hard_delete", Label: "Delete files permanently", Kind: backend.KindBool, Advanced: true,
			Help: "Delete instead of hiding old versions."},
		{Name: "versions", Label: "Include old versions", Kind: backend.KindBool, Advanced: true},
	}
}

// Capabilities implements backend.Type.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true, ObjectStore: true}
}

// Validate implements backend.Type: the field checks and a well-formed
// bucket path.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if _, err := backend.JoinRemote(cfg.Options["bucket"], ""); err != nil {
		v.Add("options.bucket", err.Error())
	}
	return v.Err()
}

// ToRclone implements backend.Type; the bucket becomes the Root.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "b2"
	r.Root = strings.Trim(cfg.Options["bucket"], "/")
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{
			Options: map[string]string{
				"account": "0012345abcdef0000000001", "bucket": "offsite/syncr", "endpoint": "https://api.backblazeb2.com",
				"download_url": "https://cdn.example.com", "hard_delete": "true", "versions": "false",
			},
			Secrets: map[string]string{"key": "K001application-key"},
		},
	}
}
