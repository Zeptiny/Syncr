// Package backend is the storage backend extension point: a Type describes
// one kind of remote (s3, sftp, local, union, ...) by its configuration
// fields, validates a remote's configuration and maps it to a typed rclone
// remote. The web UI renders remote forms from the descriptors (served by
// the API's BackendService), so adding a backend type is one descriptor
// file plus one line in internal/backend/builtin, with no frontend change.
//
// # Division of work
//
//   - A Type is the typing and UX layer: which backends are offered, their
//     fields, kinds, enums, required and secret flags, defaults and
//     cross-field rules. It does not repeat the worker's security checks.
//   - The worker (internal/worker/rcloneenv) is the security boundary: it
//     trusts nothing from the controller and rejects unsafe backends and
//     options (local files, commands, nested unions, ...). Descriptors must
//     only emit what it accepts; a cross-check test asserts this for every
//     built-in type.
//   - The remotes use case (internal/app/remotes) owns names, ownership,
//     encryption of secrets and union members: members are not part of a
//     Config.
//
// # Writing a descriptor
//
// A descriptor declares its Fields and delegates the common work to the
// helpers: CheckFields (unknown keys, secret placement, required fields,
// int/bool/enum syntax) in Validate, MapFields (fields to rclone options,
// secrets and obscured keys) in ToRclone. Fields whose Rclone name is
// NotRclone are consumed by ToRclone itself, typically a bucket or base path
// that becomes Rclone.Root. JoinRemote and JoinLocal join a root and a
// location path safely.
//
// NewRegistry checks the descriptors' static shape (see CheckDescriptor);
// internal/backend/backendtest checks their behavior.
//
// The generic field machinery (kinds, choices, configuration values,
// CheckFields and the shape checks) lives in internal/descriptor, which
// notification channels share; this package re-exports it (Choice, Config)
// and adds the rclone mapping and union members.
//
// Allowed dependencies: the standard library, internal/domain and
// internal/descriptor.
package backend
