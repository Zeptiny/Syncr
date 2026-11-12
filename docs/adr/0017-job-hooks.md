# ADR 0017: Job hooks

- Status: accepted (unreleased until v2.0.0)
- Date: 2026-11-12

## Context

A backup of a running application is often not a backup: a database's
files copied while it writes may not start again. People work around this
with a cron job that dumps the database some minutes before the backup's
schedule and hope that the order holds. They ask for what every backup
tool has in some form: run something before the job, something after it,
something when it fails.

Constraints that shaped the answer:

- Every authenticated user may run jobs on every worker (ADR 0006). Workers
  are shared; what protects a machine is its worker's configuration
  (`local_roots`), not the controller.
- A system worker runs as root or `LocalSystem`.
- The controller stores definitions, shows them to administrators and puts
  them into notifications. It must not hold anything that is dangerous to
  hold.
- Workers of earlier releases ignore spec fields they do not know.
- Schedules may target a label, and pick a worker per run.

## Decision

### Hooks are defined on the worker and named by jobs

A worker's operator defines hooks in `worker.yaml`: a name, a command as an
argument list, an optional description, timeout and environment. A job
definition (`JobDefinition.hooks`, so schedules too) names hooks for four
phases. The controller sends the names (`JobSpec.hooks`), and the worker
runs the commands its own file gives them.

No command, argument or environment value ever comes from the controller.
A user of the controller chooses among what the machine's operator
prepared, like they choose paths inside `local_roots`. Compromising a
controller account, or the controller, yields the hooks that exist and
nothing else.

What a hook is told about the job is a fixed set of `SYNCR_*` variables
whose values are IDs and enum names, restricted on the worker to letters,
digits, `.`, `_` and `-`. Paths, repository names, tags and other free text
are not passed: a hook script that interpolates them unquoted would be a
command injection that the person who wrote the script cannot see from the
worker's side. A definition's own `env` may not use the `SYNCR_` prefix.

Commands are started without a shell, through the supervisor that runs
rclone and restic (ADR 0003): the same minimal environment, stop, process
tree and death with the worker. They get none of the worker's secrets.

### Four phases, with one rule each

1. `before`, in order. The first failure skips the rest and the operation
   and fails the job (`HOOK_FAILED`). A backup of a database whose dump
   failed is worse than no backup, because it looks like one.
2. The operation.
3. `on_success`, in order, if the job succeeded so far. A failure fails the
   job; the operation's output is kept and reported.
4. `on_failure`, all of them, if the job failed (not after a cancel or a
   shutdown: nobody needs to be told that they cancelled a job). Their
   failures are recorded and change nothing.
5. `after`, all of them, always. A failure fails a job that had succeeded,
   and never changes an outcome that was already decided.

A hook succeeds when its command exits with 0. There is no "ignore
failures" switch per job: a hook that may fail without consequence says so
itself (`|| true` in its script), where the operator can see it.

Phases 1 to 3 run under the job's context, so a cancel, the time limit
(ADR 0014, which now counts from the job's start, hooks included) and a
shutdown stop a hook as they stop the tool. Phases 4 and 5 are clean-up
and run under a context that none of these end; each hook has its own
timeout (default 10 minutes, at most 24 hours), and the timer of the job's
time limit is disarmed before they start. Otherwise the user who cancels a
stuck backup would leave the application stopped.

`HOOK_FAILED` is a failure like any other for schedules, notifications and
concurrency keys. Because a job can now fail after its operation
succeeded, the worker reports the operation's output and warnings whenever
the operation succeeded, and the controller stores them whatever the final
status. A backup whose `after` hook timed out still shows its snapshot.

### What the controller knows and checks

Workers report their hooks in the Hello (`Hello.hooks`: name, description,
timeout; never the command or the environment) and the feature
`job.hooks`. The controller keeps them with the worker's info and serves
them in `WorkerInfo.hooks`, which is what the forms offer.

A hook name is validated the same everywhere
(`^[a-z0-9][a-z0-9._-]{0,62}$`), so one spelling exists and it needs no
quoting. At most 8 hooks per phase, 64 per worker.

Whether a worker can run a definition's hooks is checked in three places,
as for required features and OS-bound options:

- the API, for a job or a schedule on one worker that has reported in: a
  field error on the first place that names the missing hook;
- the scheduler: a label's run goes only to workers that define every
  hook, and a pinned worker that lacks one fails the run and pauses the
  schedule, since no later run can succeed either;
- the dispatcher's spec builder, last and authoritative: `invalid_spec`
  for a hook the worker does not define, `worker_outdated` for a worker
  without the feature. The job is never sent, so nothing runs "without its
  hooks".

The worker checks again and rejects a spec naming a hook it does not
define before anything runs.

### What is stored and shown

`JobResult.hook_runs` lists every hook that ran: name, phase, status
(succeeded, failed, timed out, cancelled), exit code, start, duration, a
message, and the last 4 KiB of its output. The controller stores them on
the job (`jobs.hook_runs`, JSON) and the job's page shows them by phase,
with the hooks that were named but did not run.

The output is the operator's own program's output. The worker cannot know
what is secret in it, so it is not scrubbed, only bounded and made valid
UTF-8; the documentation tells operators that it is shown to everyone who
can see the job. It is not put into notifications or logs.

## Consequences

- Stopping an application or dumping a database around a backup is one
  job, with one outcome and one notification.
- An operator has to touch the worker's file to add a hook and restart the
  worker. That is the price of the trust model, and the intended friction:
  adding a command that runs as root is a decision for the machine's owner.
- Every user who can run jobs on a worker can run each of its hooks, in
  any phase and for any job. An operator who defines `stop-database` has
  given that to all of them. Hooks are per worker, not per user.
- Hooks of the same name on two workers with one label are assumed to mean
  the same. Syncr cannot check it.
- A job's time limit now includes its `before` and `on_success` hooks.
- Clean-up hooks cannot be cancelled; a hanging one holds the job (and for
  restic, its repository's concurrency key) for its timeout. The default
  of 10 minutes bounds that for people who set nothing.
- A stopping worker gives its jobs about 12 seconds to end. `after` hooks
  run within that and are killed with the worker beyond it. A killed
  or crashed worker runs none. Hooks make no promise across a crash.
- Workers before this release cannot run jobs with hooks; such jobs fail
  with a message that asks for an upgrade.
- The job's page shows hooks only once the job has ended: hook runs travel
  with the result, not as events.

## Alternatives considered

- **Commands in the job definition.** What most single-user backup tools
  do, and what was asked for first. With shared workers it makes every
  account root on every worker, stores shell commands (and the passwords
  people put into them) in the controller's database, and turns a stolen
  session into remote code execution on all machines.
- **Commands in the definition, allowed per worker by a flag.** Simple, and
  equivalent to the above on every worker that sets the flag, which would
  be most of them.
- **An allowlist of programs with free arguments.** `docker` with free
  arguments is root; `sh` is everything. Argument filters are a parser to
  get wrong.
- **Hooks as steps of a workflow** (several jobs in sequence, some of them
  commands). More general, and much more to build: ordering, partial
  failure and cancel across jobs. Hooks cover the cases people have; a
  workflow could later run jobs that have hooks.
- **Live hook events** (started, output lines) over the link. They would
  let the job's page show the hook that runs. Left out to keep the first
  version to one new field in each direction; the result's list can be
  extended to events without changing what is stored.
- **Passing paths and names to hooks.** Convenient, and a quoting trap in
  every script. The hook's own definition already knows its paths.
