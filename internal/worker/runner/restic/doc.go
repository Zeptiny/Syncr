// Package restic implements the worker runners for the restic job kinds:
// restic_init, restic_backup, restic_forget, restic_prune, restic_check,
// restic_restore, restic_snapshots and restic_delete_data, and the Prober
// that answers the restic probe operations (snapshot listings, browsing a
// snapshot, creating a repository and reading its config) with the same
// engine.
//
// Every job runs one child process of the pinned restic binary through the
// supervisor in internal/worker/proc (see docs/adr/0003). The runners map the
// typed JobSpec operations to a strict allowlist of restic flags, so no
// free-form flag reaches the tool, and turn restic's --json output into
// cumulative runner.Progress snapshots and a syncrv1.JobOutput.
//
// # Repositories
//
// A repository on a remote alias is opened as "rclone:<alias>:<path>". restic
// launches rclone itself ("-o rclone.program=<rclone>", quoted for restic's
// CSV and shell-like parsing by rcloneOptions), so the environment
// that defines the remotes (RCLONE_CONFIG_<ALIAS>_*, produced by RemoteEnv)
// is part of restic's environment; rclone runs with "--config=" so no
// rclone.conf of the worker's user is ever read. An empty remote is a local
// repository whose path must pass PathPolicy, like backup sources and restore
// targets.
//
// # Restores
//
// A restore names what to restore as literal snapshot tree paths ("/srv/x",
// or "/C/Users/x" for a snapshot made on Windows), never as patterns: the
// runner escapes them into --include patterns (includePattern), or passes a
// single folder as "<snapshot>:<folder>" so its contents land directly in
// the target. Existing files are handled as the spec's overwrite behavior
// says; the controller always sets it.
//
// # Deleting a repository's data
//
// restic_delete_data (delete.go) removes restic's own files at a
// repository's location and nothing else there. It first verifies that the
// location still holds the expected repository ("cat config" with the
// stored password, compared with the expected ID) and that no other client
// holds a lock; then it deletes, with the pinned rclone, the files named
// like restic's in locks/, snapshots/, index/, data/ and keys/ (64 hex
// digits; in data/ inside sub-folders named by 2 hex digits, selected with
// rclone filter rules), the config last, and removes the folders it
// emptied. Anything else in those folders, another repository stored there
// included, stays and is named in a warning. A run that stopped part-way
// can be repeated when the spec says so (resume): with the keys gone only
// the config is left to delete, and with nothing left it succeeds at once
// (already_gone). A first run that finds nothing fails instead: the
// repository is elsewhere. The folder itself is removed only when asked
// to, when it is empty and when it is not the remote's root.
//
// # Secrets
//
// The repository password is passed in RESTIC_PASSWORD, never as an argument.
// An environment variable is preferred over RESTIC_PASSWORD_FILE because the
// worker keeps no secret on disk (docs/adr/0003): the variable is visible
// only to the child's own user (/proc/<pid>/environ is mode 0400), whereas a
// password file would have to be written, and reliably wiped, on every job.
// restic hands its environment to the rclone it starts, which is the same
// trust domain. Every error message is scrubbed of the repository password
// and of the remotes' secret options before it leaves the package, in the
// forms tools print them (see internal/worker/runner/scrub) and, for
// obscure_keys, in the obscured form the child was given.
//
// # Outcomes
//
// Exit codes are mapped in ExitError and jobErrorCode: 10, 11 and 12 become
// the wire codes REPOSITORY_NOT_FOUND, REPOSITORY_LOCKED and WRONG_PASSWORD
// (and the Err* sentinels), everything else EXECUTION_FAILED. A backup that
// exits with 3 ("some source data could not be read") still created a
// snapshot: the runner returns that snapshot's output with a nil error,
// ExitCode 3, a non-zero Stats.Errors and Result.Warnings ("N items could not
// be read; the snapshot is incomplete" plus the first reasons restic gave,
// scrubbed and bounded by runner.LimitWarnings). The job status stays
// SUCCEEDED. A restic that exits 0 while a process it started (rclone) holds
// the output pipes open past the supervisor's grace period
// (exec.ErrWaitDelay) is judged by its own output, like any other exit 0.
//
// Per-repository serialization is the controller's job; a lock conflict
// (exit 11) is reported as ErrRepositoryLocked.
//
// Allowed dependencies: the standard library, internal/worker packages and
// the generated protobuf code. It must not import the rclone runner.
package restic
