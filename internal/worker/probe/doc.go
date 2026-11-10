// Package probe runs the worker's probes: short, synchronous requests the
// controller sends outside the job slots, such as listing a folder, testing
// a remote or reading a restic repository (see link.proto, "Probes").
//
// The Executor only dispatches: each operation is implemented where its
// tool lives (the restic and rclone Probers, the local path policy), behind
// the small interfaces declared here. The Executor adds what every probe
// needs: a bound on concurrent probes (more are refused as BUSY at once,
// never queued), a timeout with a hard cap, bounds on listing sizes and on
// the encoded result, and the mapping of failures to wire error codes.
//
// A probe never returns a Go error: its outcome, including failures and
// cancellation, is a ProbeResult. Nothing about a probe is stored, and
// probe specs, which carry secrets, are never logged.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/link, internal/worker/localfs and the runner contract
// (internal/worker/runner, for its error codes; not the concrete runners).
package probe
