// Package webdav is the backend descriptor of WebDAV servers such as
// Nextcloud and ownCloud (rclone backend "webdav").
package webdav

import (
	"net/url"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the webdav backend type.
type Type struct{}

var _ backend.Exampler = Type{}

// ID implements backend.Type.
func (Type) ID() string { return "webdav" }

// Label implements backend.Type.
func (Type) Label() string { return "WebDAV" }

// Description implements backend.Type.
func (Type) Description() string {
	return "A WebDAV server: Nextcloud, ownCloud, SharePoint or any other."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "url", Label: "URL", Kind: backend.KindString, Required: true,
			Placeholder: "https://cloud.example.com/remote.php/dav/files/alice", Help: "The WebDAV root; job paths are relative to it."},
		{Name: "vendor", Label: "Vendor", Kind: backend.KindEnum, Default: "other", Choices: []backend.Choice{
			{Value: "other", Label: "Other"}, {Value: "nextcloud", Label: "Nextcloud"}, {Value: "owncloud", Label: "ownCloud"},
			{Value: "sharepoint", Label: "SharePoint Online"}, {Value: "sharepoint-ntlm", Label: "SharePoint (NTLM)"},
			{Value: "rclone", Label: "rclone serve webdav"},
		}},
		{Name: "user", Label: "User", Kind: backend.KindString},
		{Name: "pass", Label: "Password", Kind: backend.KindString, Secret: true, Obscure: true},
		{Name: "bearer_token", Label: "Bearer token", Kind: backend.KindString, Secret: true, Advanced: true,
			Help: "Instead of a user and password."},
	}
}

// Capabilities implements backend.Type.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true}
}

// Validate implements backend.Type: the field checks and an http(s) URL.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if raw := cfg.Options["url"]; raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			v.Add("options.url", "must be an http:// or https:// URL")
		}
	}
	return v.Err()
}

// ToRclone implements backend.Type.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "webdav"
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{
			Options: map[string]string{"url": "https://cloud.example.com/remote.php/dav/files/alice", "vendor": "nextcloud", "user": "alice"},
			Secrets: map[string]string{"pass": "app-password", "bearer_token": "token"},
		},
	}
}
