package schedules

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Schedule runs a job definition on a cron schedule.
type Schedule struct {
	ID      domain.ScheduleID
	OwnerID domain.UserID
	// Name is 1 to 64 characters, unique per owner (case-insensitive).
	Name        string
	Description string

	// Cron is the normalized cron expression (cronexpr.Normalize) and
	// Timezone its IANA zone.
	Cron     string
	Timezone string

	// Kind, Definition and RemoteIDs are the job each run launches, as
	// jobs.CreateParams takes them: the kind, the encoded secret-free
	// definition (protojson of syncr.v1.JobDefinition, checked by the API
	// with internal/jobkind) and the owner's remotes it refers to, sorted by
	// ID without duplicates.
	Kind       string
	Definition []byte
	RemoteIDs  []domain.RemoteID
	// SearchText is the definition's search terms (jobkind.Refs), one per
	// line, for the launched jobs' search text.
	SearchText string

	// Target is where the jobs run.
	Target Target
	// Missed is what happens to a run whose worker is offline.
	Missed MissedPolicy
	// Overlap is what happens to a run that is due while one is active.
	Overlap Overlap

	Enabled bool
	// PausedReason is the reason code of an automatic pause (e.g.
	// ReasonWorkerRemoved); "" if the controller did not pause it. Enabling
	// clears it.
	PausedReason string

	// Contacts are the linked contacts and their events, by contact ID.
	Contacts []ContactLink

	// NextRunAt is when the next occurrence is due; zero while disabled or
	// if the expression has no further occurrence. LastFiredAt is when an
	// occurrence last fired; zero if never. They are the scheduler's fire
	// state: compare-and-swapped by Repository.FireSchedule, recomputed by
	// a user edit only when Cron, Timezone or Enabled changes.
	NextRunAt   time.Time
	LastFiredAt time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	// Version increases with every change of the configuration (not with
	// fires); it is the optimistic concurrency token of
	// Repository.UpdateSchedule. A created schedule has version 1.
	Version int64
}

// Target is where a schedule's jobs run: exactly one of WorkerID and Label
// is set.
type Target struct {
	// WorkerID is one worker. It must exist and not be removed when the
	// schedule is saved; a schedule whose worker is removed is paused.
	WorkerID domain.WorkerID
	// Label selects the least busy online worker with the label that can
	// run the kind (domain.ValidLabel).
	Label string
}

// MissedAction is what happens to a run whose worker is offline when it is
// due. The string values are stable: they are stored.
type MissedAction string

// Missed-run actions.
const (
	// MissedRunLate waits for the worker up to MissedPolicy.MaxDelay.
	MissedRunLate MissedAction = "run_late"
	// MissedSkip marks the run missed after SkipGrace.
	MissedSkip MissedAction = "skip"
)

// Valid reports whether a is a known action.
func (a MissedAction) Valid() bool { return a == MissedRunLate || a == MissedSkip }

// MissedPolicy says how long a due run may wait for its worker.
type MissedPolicy struct {
	Action MissedAction
	// MaxDelay is how long after its scheduled time a run_late run may
	// still start, at most MaxMaxDelay; 0 means until the next occurrence.
	MaxDelay time.Duration
}

// Overlap is what happens when a run is due while another run of the
// schedule is active (launching, or its job queued, dispatched or running).
// The string values are stable: they are stored.
type Overlap string

// Overlap policies.
const (
	// OverlapSkip records the new run as missed (reason overlap).
	OverlapSkip Overlap = "skip"
	// OverlapQueue launches the new run when the active one has finished.
	OverlapQueue Overlap = "queue"
	// OverlapAllow launches the new run alongside the active one.
	OverlapAllow Overlap = "allow"
)

// Valid reports whether o is a known policy.
func (o Overlap) Valid() bool { return o == OverlapSkip || o == OverlapQueue || o == OverlapAllow }

// ContactLink links one of the owner's contacts to a schedule.
type ContactLink struct {
	ContactID domain.ContactID
	// Events are the events to notify, normalized, a non-empty subset of
	// domain.ScheduleEvents.
	Events domain.EventSet
}

// RunStatus is where a run is. The string values are stable: they are
// stored.
type RunStatus string

// Run statuses.
const (
	// RunPending waits for its worker or for an active run to finish.
	RunPending RunStatus = "pending"
	// RunLaunching has reserved its job ID; the job is being created.
	RunLaunching RunStatus = "launching"
	// RunLaunched created its job. Terminal.
	RunLaunched RunStatus = "launched"
	// RunMissed could not start in time. Terminal.
	RunMissed RunStatus = "missed"
	// RunFailed could not start at all. Terminal.
	RunFailed RunStatus = "failed"
	// RunCancelled was open when the schedule was paused. Terminal.
	RunCancelled RunStatus = "cancelled"
)

// Valid reports whether s is a known status.
func (s RunStatus) Valid() bool {
	switch s {
	case RunPending, RunLaunching, RunLaunched, RunMissed, RunFailed, RunCancelled:
		return true
	}
	return false
}

// Open reports whether s is pending or launching. A schedule has at most
// one pending run (and any number of launching ones).
func (s RunStatus) Open() bool { return s == RunPending || s == RunLaunching }

// Run triggers.
const (
	// TriggerCron: an occurrence of the cron expression.
	TriggerCron = "cron"
	// TriggerManual: RunNow.
	TriggerManual = "manual"
)

// Reason codes of runs that did not launch (Run.Reason), and of automatic
// pauses (Schedule.PausedReason).
const (
	ReasonWorkerOffline         = "worker_offline"
	ReasonOverlap               = "overlap"
	ReasonControllerUnavailable = "controller_unavailable"
	ReasonRemoteUnavailable     = "remote_unavailable"
	ReasonInvalidDefinition     = "invalid_definition"
	ReasonWorkerRemoved         = "worker_removed"
	ReasonNoWorkerWithLabel     = "no_worker_with_label"
	ReasonOwnerDisabled         = "owner_disabled"
	ReasonDisabled              = "disabled"
)

// Run is one run of a schedule: one occurrence (or several merged ones)
// waiting for, or having launched, its job.
type Run struct {
	ID         domain.ScheduleRunID
	ScheduleID domain.ScheduleID
	// Trigger is TriggerCron or TriggerManual.
	Trigger string
	// ScheduledFor is the occurrence that opened the run (for a manual run,
	// when it was requested); Deadline the latest time it may still start.
	ScheduledFor time.Time
	Deadline     time.Time
	// Coalesced counts later occurrences merged into the run (and
	// occurrences that passed while the controller was down).
	Coalesced int

	Status RunStatus
	// Reason is a reason code (Reason* constants) for missed, failed and
	// cancelled runs; Message its details for display.
	Reason  string
	Message string

	// WorkerID and JobID are set from launching on: the job ID is reserved
	// before the job is created, so a retried launch creates it once.
	WorkerID domain.WorkerID
	JobID    domain.JobID
	// JobStatus is the status of the run's job, read from the jobs (empty
	// while there is none). Repositories fill it in; it is never written.
	JobStatus domain.JobStatus

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Limits and policy constants.
const (
	// MaxNameLen bounds a schedule name, in characters.
	MaxNameLen = 64
	// MaxDescriptionLen bounds a description, in characters.
	MaxDescriptionLen = 1000
	// MaxMaxDelay bounds MissedPolicy.MaxDelay.
	MaxMaxDelay = 7 * 24 * time.Hour
	// SkipGrace is the deadline of a skip-policy run after its scheduled
	// time.
	SkipGrace = 5 * time.Minute
	// ManualUntilNext is how long a run of a run_late schedule with "until
	// next" (MaxDelay 0) may wait when the expression has no next
	// occurrence (a manual run's deadline counts from the request).
	ManualUntilNext = 24 * time.Hour
	// MaxCoalesce bounds the occurrences counted for one fire
	// (cronexpr.Expr.Latest).
	MaxCoalesce = 1000
	// MaxContacts bounds the contacts linked to a schedule.
	MaxContacts = 20
)

// Input is the configuration of a schedule to create or update, as the API
// passes it: the definition already checked and encoded (jobkind.Check,
// jobkind.Marshal), so this package stays free of protobuf.
type Input struct {
	Name        string
	Description string
	Cron        string
	Timezone    string
	Target      Target
	Missed      MissedPolicy
	// Overlap "" means OverlapSkip.
	Overlap Overlap
	Enabled bool

	Kind       string
	Definition []byte
	// RemoteIDs and SearchTerms are the definition's references
	// (jobkind.Refs); the remotes must be live remotes of the actor.
	RemoteIDs   []domain.RemoteID
	SearchTerms []string

	// Contacts must be the actor's contacts, each once.
	Contacts []ContactLink
}

// ListFilter selects schedules. Zero fields do not filter.
type ListFilter struct {
	// OwnerID keeps only this user's schedules (the service always sets it
	// to the actor).
	OwnerID domain.UserID
	// RemoteID keeps only schedules whose RemoteIDs contain it.
	RemoteID domain.RemoteID
	// ContactID keeps only schedules linked to this contact.
	ContactID domain.ContactID
}

// RunFilter selects runs. Zero fields do not filter.
type RunFilter struct {
	ScheduleID domain.ScheduleID
	// OwnerID keeps only runs of this user's schedules.
	OwnerID  domain.UserID
	Statuses []RunStatus
	// ScheduledFrom (inclusive) and ScheduledUntil (exclusive) bound
	// ScheduledFor.
	ScheduledFrom  time.Time
	ScheduledUntil time.Time
	// Before is a keyset cursor: only runs with ID < Before.
	Before domain.ScheduleRunID
	// Limit is the number of runs to return; negative means no limit
	// (repositories), 0 means DefaultPageSize (the service).
	Limit int
}

// Page sizes of run lists.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// RunPage is one page of a schedule's runs, newest first.
type RunPage struct {
	Runs []Run
	// Next is the cursor (RunFilter.Before) of the next page; zero on the
	// last page.
	Next domain.ScheduleRunID
}

// Ref names a schedule for display (e.g. a job's schedule).
type Ref struct {
	ID   domain.ScheduleID
	Name string
}

// Upcoming is the next occurrence of an enabled schedule (dashboard).
type Upcoming struct {
	ScheduleID domain.ScheduleID
	Name       string
	At         time.Time
	Target     Target
}

// Preview is the result of checking a cron expression (PreviewSchedule).
type Preview struct {
	// NextRuns are the next occurrences, oldest first.
	NextRuns []time.Time
	// Normalized is the expression as stored.
	Normalized string
}

// Fire is one fire of a due schedule (Repository.FireSchedule): the
// compare-and-swap of its NextRunAt and the run that holds the
// occurrence(s).
type Fire struct {
	ID domain.ScheduleID
	// ExpectedNext is the NextRunAt the scheduler read; NewNext replaces it
	// (zero: no further occurrence).
	ExpectedNext time.Time
	NewNext      time.Time
	// Run is the run to insert; its CreatedAt is the fire time (it becomes
	// LastFiredAt). A terminal run (missed, reason overlap) is always
	// inserted.
	Run Run
	// Merge merges the fire into the schedule's pending run, if it has one
	// and its Deadline is after Run.CreatedAt, instead of inserting Run;
	// the pending run keeps its deadline. A pending run whose Deadline is
	// not after Run.CreatedAt is superseded instead: it is ended missed
	// with MissedReason and MissedMessage, and Run is inserted.
	Merge bool
	// MissedReason and MissedMessage end a superseded pending run.
	MissedReason, MissedMessage string
}

// Toggle is the change of Repository.SetScheduleEnabled.
type Toggle struct {
	Enabled bool
	// NextRunAt is the next occurrence when enabling (zero if none); it is
	// ignored (cleared) when disabling.
	NextRunAt time.Time
	// PausedReason is recorded when disabling ("" for a pause by the
	// user); enabling clears it.
	PausedReason string
	// At is the time of the change (UpdatedAt of the schedule and of a
	// cancelled run).
	At time.Time
}

// RunUpdate is a compare-and-swap of a run's status (Repository.UpdateRun).
type RunUpdate struct {
	ID domain.ScheduleRunID
	// From is the status the caller read; To the new one.
	From, To RunStatus
	Reason   string
	Message  string
	// WorkerID and JobID are written when not zero (launching sets them);
	// zero keeps the stored values.
	WorkerID domain.WorkerID
	JobID    domain.JobID
	// At is the new UpdatedAt.
	At time.Time
	// PauseSchedule also disables the run's schedule in the same
	// transaction (auto-pause): Enabled false, NextRunAt zero, PausedReason
	// Reason, Version + 1.
	PauseSchedule bool
}
