// Package tools provisions the pinned rclone and restic binaries a worker
// runs jobs with.
//
// A Resolver walks a chain of Providers and returns the first binary that is
// verified to be the pinned version:
//
//  1. ConfiguredProvider: an explicit path chosen by the operator;
//  2. BundledProvider: a binary shipped next to the worker executable;
//  3. ManagedProvider: an install in the worker's tools directory, downloaded
//     on demand by an Installer;
//  4. PathProvider: a binary found on PATH.
//
// Every provider accepts a binary only if `<tool> version` reports exactly
// the pinned version; the managed install is additionally verified against
// the SHA-256 recorded when it was installed.
//
// The pins live in the Manifest (see DefaultManifest). Each archive's
// SHA-256 is embedded in Go source and reviewed with the code; the worker
// never trusts a checksum fetched at runtime. The Installer streams an
// archive to a temporary file, verifies its digest before opening it, and
// extracts only the single expected binary under a size cap. Concurrent
// installs (several workers, or two `setup` runs) are serialized with an
// OS-level lock file (internal/worker/filelock).
//
// Installer.Prune removes the managed installs of versions that are no
// longer pinned. Only setup and an upgrade that passed its health check
// call it; a running worker never does, so a worker rolled back to its
// previous release still finds that release's tools.
//
// ExtractFile is the same member-by-name extraction for zip and tar.gz
// archives, which the worker upgrade uses on its own verified release
// archive.
//
// Allowed dependencies: the standard library, internal/worker/proc and
// internal/worker/filelock.
package tools
