// Package link holds worker-link logic shared by the controller and the
// worker: job kinds and probe ops derived from the wire contract, error code
// names, the link features workers report in their Hello (Milestone 5), and
// the authorization headers (see docs/adr/0002-worker-link.md and
// proto/syncr/v1/link.proto).
//
// Allowed dependencies: the standard library, internal/domain and the
// generated protobuf code. It must not import controller-, worker- or
// adapter-specific packages, because both sides of the link use it.
package link
