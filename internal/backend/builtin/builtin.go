// Package builtin lists the backend types built into Syncr. Adding a type is
// one descriptor package plus one line here.
package builtin

import (
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/b2"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/local"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/s3"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/sftp"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/union"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/webdav"
)

// Types returns the built-in backend types, for backend.NewRegistry. The
// slice is the caller's.
func Types() []backend.Type {
	return []backend.Type{
		b2.Type{},
		local.Type{},
		s3.Type{},
		sftp.Type{},
		union.Type{},
		webdav.Type{},
	}
}
