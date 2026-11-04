// Package jobs owns the job lifecycle: creating jobs (Milestone 2: an
// explicit worker and a complete, encrypted spec), reading and listing them,
// canceling them, live progress, and every status change the dispatcher
// derives from the worker link.
//
// Every status change goes through this service, which
//
//   - checks it against the domain state machine (domain.JobStatus) and the
//     link preconditions (the job belongs to the calling worker and, for a
//     dispatched or running job, to the worker epoch that accepted it);
//   - stores it with an optimistic, version-checked update
//     (Repository.UpdateJob), retrying on a lost race, so concurrent changes
//     from the API and the dispatcher serialize per job;
//   - publishes the new job to the live broker, wakes the worker's command
//     long poll when its commands change (re-queue, cancel request), and
//     calls the notifier when a job ends failed or interrupted.
//
// Lifecycle calls are idempotent: a call whose precondition no longer holds
// (a duplicate event, a lost race, a job already finished) changes nothing
// and reports changed == false without an error.
//
// # Secrets
//
// The JobSpec contains remote credentials. It reaches this package already
// encoded (the protobuf wire form; this package does not interpret it) and
// is stored encrypted with the Cipher, bound to its record by the associated
// data "job-spec:<job id>". Only the dispatcher reads it back (Spec), to send
// it to the worker.
//
// # Progress
//
// Progress samples are cumulative counters. Every sample is published live;
// one per Config.SampleInterval per job is stored (rate limiting happens in
// memory before storage), plus the final counters when the job ends. Rates
// are derived by readers (delta bytes / delta time).
//
// Allowed dependencies: the standard library and internal/domain; every
// other collaborator is a port declared in ports.go.
package jobs
