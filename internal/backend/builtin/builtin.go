// Package builtin lists the backend types built into Syncr. Adding a type is
// one descriptor package plus one line here; a type on a new rclone backend
// also needs that backend reviewed and added to the worker's allowlist
// (rcloneenv.SupportedBackends), which the worker's cross-check test
// enforces, and a type that older workers refuse needs its link feature in
// internal/jobkind/features.go.
package builtin

import (
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/azureblob"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/b2"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/crypt"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/ftp"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/gcs"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/local"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/s3"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/sftp"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/smb"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/union"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/webdav"
)

// Types returns the built-in backend types, for backend.NewRegistry. The
// slice is the caller's.
func Types() []backend.Type {
	return []backend.Type{
		azureblob.Type{},
		b2.Type{},
		crypt.Type{},
		ftp.Type{},
		gcs.Type{},
		local.Type{},
		s3.Type{},
		sftp.Type{},
		smb.Type{},
		union.Type{},
		webdav.Type{},
	}
}
