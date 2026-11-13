// Package probe runs the worker's probes: short, synchronous requests the
// controller sends outside the job slots, such as listing a folder, testing
// a remote or reading a restic repository (see link.proto, "Probes").
//
// The Executor only dispatches: each operation is implemented where its
// tool lives (the restic and rclone Probers, the local path policy), behind
// the small interfaces declared here. The one operation without a tool,
// sftp_host_keys, is answered here by SSHScanner, which speaks just enough
// SSH to read a server's host keys. The Executor adds what every probe
// needs: a bound on concurrent probes (more are refused as BUSY at once,
// never queued), a timeout with a hard cap (link.MaxProbeTimeout, shared
// with the controller), bounds on listing sizes and on the encoded result,
// and the mapping of failures to wire error codes.
//
// The heavy operations (link.HeavyProbeOps: measuring a folder, searching
// and comparing snapshots) walk whole trees. Each also holds one of the
// MaxHeavy slots (one by default), so a worker measures or searches one
// thing at a time, and ends at a soft deadline before the probe's timeout
// (SoftMargin): it then answers OK with what it found and complete=false,
// so that a long walk still returns something within a proxy's timeout.
// Their results are bounded too: child folders by MaxEntries, matches and
// changes by the request (DefaultMatches, DefaultChanges) and the caps
// MaxMatches and MaxChanges.
//
// Every operation stops with its context: the tools' child processes are
// ended, and a local listing or measurement checks its context between
// batches of directory entries and between stats. A filesystem call
// blocked in the kernel (a hung network mount) cannot be interrupted, so a
// local operation runs in a goroutine that holds the probe's slots: the
// probe answers at its deadline or cancellation, and the slots come back
// when the call returns. Stuck operations therefore never exceed the
// slots, and while every slot is stuck the worker answers BUSY.
//
// A probe never returns a Go error: its outcome, including failures and
// cancellation, is a ProbeResult. Nothing about a probe is stored, and
// probe specs, which carry secrets, are never logged.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/link, internal/worker/localfs, the runner contract
// (internal/worker/runner, for its error codes; not the concrete runners)
// and golang.org/x/crypto/ssh (not its subpackages).
package probe
