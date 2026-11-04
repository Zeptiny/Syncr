// Package restic implements the worker runners for the restic job kinds:
// restic_init, restic_backup, restic_forget, restic_prune, restic_check,
// restic_restore and restic_snapshots.
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
// launches rclone itself ("-o rclone.program=<rclone>"), so the environment
// that defines the remotes (RCLONE_CONFIG_<ALIAS>_*, produced by RemoteEnv)
// is part of restic's environment; rclone runs with "--config=" so no
// rclone.conf of the worker's user is ever read. An empty remote is a local
// repository whose path must pass PathPolicy, like backup sources and restore
// targets.
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
// and of the remotes' secret options before it leaves the package.
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
// SUCCEEDED.
//
// Per-repository serialization is the controller's job; a lock conflict
// (exit 11) is reported as ErrRepositoryLocked.
//
// Allowed dependencies: the standard library, internal/worker packages and
// the generated protobuf code. It must not import the rclone runner.
package restic
