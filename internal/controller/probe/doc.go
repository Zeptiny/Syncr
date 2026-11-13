// Package probe runs worker probes for the controller's use cases
// (Milestone 5; docs/adr/0012-worker-probes.md): short, synchronous
// requests a worker answers outside its job slots, such as listing a
// folder, testing a remote or reading a restic repository's snapshots.
//
// It crosses use cases (remotes, restic repositories, workers, jobs and the
// worker link), so it lives in the controller layer, like dispatch and the
// scheduler:
//
//   - Runner builds a probe's spec, picks the worker, sends the probe
//     through a Transport (*dispatch.ProbeBroker) and maps the outcome. It
//     implements restic.Prober for the repository use cases.
//   - Service is the API-facing part (api.ProbeService): browsing a
//     worker's folders, browsing a remote, testing a remote, fetching
//     an SFTP server's host keys (GetSftpHostKeys, Milestone 6: the worker
//     only runs SSH handshakes and sends no credentials; the controller
//     validates the address first and checks the keys it gets back), and
//     (Milestone 7) measuring a worker's or a remote's folder and asking a
//     remote for its usage. It speaks the generated response types,
//     because the API cannot import controller packages.
//   - The heavy probes (Milestone 7, link.HeavyProbeOps, scan.go):
//     measure_local and measure_remote (Service), restic_measure,
//     restic_find and restic_diff (Runner, for the repository use cases).
//     They run with Config.ScanTimeout; the worker stops them at a soft
//     deadline before it and answers OK with what it found
//     (complete=false), which is passed through as a success. A worker
//     runs one at a time and refuses another as BUSY.
//   - DataDeleter starts a repository's restic_delete_data job
//     (restic.Deletions). It is here because it shares the worker pick and
//     needs the job kinds and the job service, which app packages cannot
//     use.
//
// # Every probe
//
//  1. The caller's rate limit (Limiter, keyed by user ID).
//  2. Owner-scoped resolution: remotes are resolved for the caller's own
//     user (another user's is domain.ErrNotFound, admins included), and a
//     repository arrives as a restic.Target its service resolved the same
//     way. jobkind.Locate builds the spec's remotes and locations exactly
//     as job specs are built; remote paths cannot contain "..", local
//     paths are checked by the worker against its local_roots.
//  3. The worker: an explicitly chosen one is validated (CheckExplicit); a
//     remote on a worker's local filesystem needs one; otherwise Pick
//     takes an online worker that can run the operation, preferring the
//     worker of the repository's newest succeeded job, then the least busy
//     one.
//  4. The probe is sent with a fresh ID and the configured timeout:
//     Config.Timeout for listings and usage, ResticTimeout for the other
//     restic operations, ScanTimeout for the heavy ones.
//  5. Every message that leaves this package is scrubbed of the spec's
//     secrets (redact.Scrub), although workers scrub theirs too.
//  6. Failures are mapped to domain errors (see failure).
//
// Specs hold secrets and are never logged or stored.
//
// # Errors
//
//	Condition                                         Error
//	foreign or missing remote or worker               domain.ErrNotFound
//	the worker reports PATH_NOT_ALLOWED               *domain.ValidationError on "path"
//	the worker reports NOT_FOUND                      *domain.ValidationError on "path"
//	WRONG_PASSWORD                                    domain.ErrFailedPrecondition
//	REPOSITORY_NOT_FOUND                              domain.ErrFailedPrecondition
//	TIMEOUT, BUSY, cancelled, no answer, offline      domain.ErrUnavailable
//	BUSY of a heavy probe, its heavy slot taken       domain.ErrUnavailable ("is already measuring
//	  (link.ProbeBusyHeavy)                             or searching; try again in a moment")
//	BUSY, normal slots full                           domain.ErrUnavailable ("is busy with other
//	                                                    requests; try again in a moment")
//	an OK answer without the operation's output       domain.ErrUnavailable
//	a remote on a worker's disk without a worker      *domain.ValidationError on "worker_id"
//	measuring a worker's folder without worker or
//	  path                                            *domain.ValidationError on "worker_id", "path"
//	an outdated worker, no usable worker              domain.ErrFailedPrecondition
//	any other failure the worker reports              domain.ErrFailedPrecondition (scrubbed, clipped)
//	host keys: unreachable, not SSH, no answer in
//	  time (TEMPORARY, EXECUTION_FAILED, TIMEOUT)     domain.ErrFailedPrecondition (the worker's message)
//	host keys: a malformed host or port               *domain.ValidationError on "host" or "port"
//	too many probes of one user                       domain.ErrRateLimited
//
// A failure the worker reported also carries a *restic.ProbeError (code
// and scrubbed message; errors.As), which the repository use cases turn
// into field errors (a NOT_FOUND of a missing snapshot becomes an error on
// the snapshot's field there, not on "path").
//
// GetRemoteUsage of a remote on a worker's disk validates the chosen
// worker and answers usage.unsupported without a probe: a worker reports
// no usage for its own folders.
//
// Allowed dependencies: the standard library, protobuf, the generated
// code, internal/link, internal/domain, internal/jobkind, internal/backend
// (value types), internal/platform/redact and the app services' types;
// collaborators are the ports in ports.go. No adapters and no dispatch:
// cmd/ wires the broker in as the Transport.
package probe
