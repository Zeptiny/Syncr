// Package local is the backend descriptor of a directory on the worker's
// own filesystem. It maps to no rclone backend: locations on a local remote
// are plain local paths, which the worker checks against its local_roots.
package local

import (
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the local backend type.
type Type struct{}

var _ backend.Exampler = Type{}

// ID implements backend.Type.
func (Type) ID() string { return "local" }

// Label implements backend.Type.
func (Type) Label() string { return "Local directory" }

// Description implements backend.Type.
func (Type) Description() string {
	return "A directory on the worker that runs the job, inside its local roots."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "path", Label: "Path", Kind: backend.KindString, Required: true, Rclone: backend.NotRclone,
			Placeholder: "/srv/data", Help: `Absolute path on the worker, e.g. /srv/data or D:\Data.`},
	}
}

// Capabilities implements backend.Type.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{Local: true, UnionMember: true, ResticRepository: true}
}

// Validate implements backend.Type: the field checks and an absolute POSIX
// or Windows path.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	if p := cfg.Options["path"]; p != "" {
		if _, err := backend.JoinLocal(p, ""); err != nil {
			v.Add("options.path", err.Error())
		}
	}
	return v.Err()
}

// ToRclone implements backend.Type: no backend, the cleaned path as Root.
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	root, err := backend.JoinLocal(cfg.Options["path"], "")
	if err != nil {
		return backend.Rclone{}, err
	}
	r.Root = root
	return r, nil
}

// Examples implements backend.Exampler.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{Options: map[string]string{"path": "/srv/photos"}},
		{Options: map[string]string{"path": `D:\Backups`}},
	}
}
