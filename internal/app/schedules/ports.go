package schedules

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists schedules, their remote and contact links and their
// runs. Implementations must be safe for concurrent use. Every method is
// atomic on its own (a schedule and its link rows change in one
// transaction; FireSchedule is one transaction); no use case needs a
// transaction spanning several calls. Errors wrap the domain sentinels
// named below; anything else is an internal error. Returned values must not
// alias storage. Every method returning schedules returns them with their
// RemoteIDs (sorted by ID) and Contacts (sorted by contact ID); every
// method returning runs fills in Run.JobStatus from the jobs.
type Repository interface {
	// CreateSchedule inserts s with its remote and contact links (Version
	// is stored as given, normally 1). ErrAlreadyExists if s.ID is taken,
	// the owner has a schedule with the same name, case-insensitively, or a
	// contact is linked twice (primary key of schedule_contacts);
	// ErrNotFound if the owner, the target worker (removed ones exist), a
	// remote (deleted ones exist) or a contact does not exist (foreign
	// keys). ErrInvalidArgument if both or neither of Target.WorkerID and
	// Target.Label are set, or a disabled schedule has a NextRunAt (checks).
	CreateSchedule(ctx context.Context, s Schedule) error

	// GetSchedule returns a schedule. ErrNotFound if missing.
	GetSchedule(ctx context.Context, id domain.ScheduleID) (Schedule, error)

	// GetSchedules returns the schedules of ids that exist, by ID; missing
	// ones are omitted and duplicate IDs are returned once.
	GetSchedules(ctx context.Context, ids []domain.ScheduleID) ([]Schedule, error)

	// ListSchedules returns the schedules matching f, by lower-cased name,
	// then ID.
	ListSchedules(ctx context.Context, f ListFilter) ([]Schedule, error)

	// UpdateSchedule is an optimistic, version-checked update of the
	// configuration: if the stored Version equals s.Version, it writes
	// Name, Description, Cron, Timezone, Kind, Definition, RemoteIDs,
	// SearchText, Target, Missed, Overlap, Enabled, PausedReason, Contacts
	// and UpdatedAt, and NextRunAt only if reschedule (the service
	// recomputed it because Cron, Timezone or Enabled changed); otherwise
	// the stored NextRunAt is kept, so an edit racing a fire cannot rewind
	// it. LastFiredAt, ID, OwnerID and CreatedAt are never written. It sets
	// Version = s.Version + 1 and returns the stored schedule. When it
	// changes Enabled to false it also cancels the open runs (status
	// cancelled, reason ReasonDisabled, UpdatedAt = s.UpdatedAt) in the
	// same transaction. ErrConflict if the version differs, ErrNotFound if
	// missing (or a new link's row is missing), ErrAlreadyExists if the new
	// name is taken, ErrInvalidArgument as for CreateSchedule (a disabled
	// result with a NextRunAt included).
	UpdateSchedule(ctx context.Context, s Schedule, reschedule bool) (Schedule, error)

	// SetScheduleEnabled pauses or resumes a schedule without a version
	// check: it sets Enabled = t.Enabled, NextRunAt = t.NextRunAt (zero when
	// disabling), PausedReason = t.PausedReason ("" when enabling),
	// UpdatedAt = t.At and Version + 1, and when disabling cancels the open
	// runs (status cancelled, reason ReasonDisabled, UpdatedAt = t.At) in the
	// same transaction. It returns the stored schedule. ErrNotFound if
	// missing.
	SetScheduleEnabled(ctx context.Context, id domain.ScheduleID, t Toggle) (Schedule, error)

	// DeleteSchedule deletes a schedule with its links and runs; jobs and
	// deliveries that refer to it keep existing with their ScheduleID
	// cleared. ErrNotFound if missing.
	DeleteSchedule(ctx context.Context, id domain.ScheduleID) error

	// ListDueSchedules returns at most limit enabled schedules whose
	// NextRunAt is set and <= now, by NextRunAt, then ID (index
	// schedules_due_idx).
	ListDueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error)

	// FireSchedule fires a due schedule, in one transaction:
	//
	//  1. a compare-and-swap of the fire state: if the schedule is enabled
	//     and its NextRunAt equals f.ExpectedNext, it sets NextRunAt =
	//     f.NewNext and LastFiredAt = f.Run.CreatedAt (Version and
	//     UpdatedAt are untouched); otherwise ErrConflict and nothing
	//     changes (another tick or controller fired it, or the user edited
	//     or paused it; the caller re-reads on its next tick);
	//  2. if f.Merge and the schedule has a pending run whose Deadline is
	//     after f.Run.CreatedAt, that run absorbs the fire: Coalesced +=
	//     f.Run.Coalesced + 1, UpdatedAt = f.Run.CreatedAt; its Deadline
	//     is fixed when it opened and does not move. A launching run
	//     absorbs nothing: its job is being created;
	//  3. if f.Merge and the schedule has a pending run whose Deadline is
	//     not after f.Run.CreatedAt, that run is superseded: Status
	//     missed, Reason f.MissedReason, Message f.MissedMessage, UpdatedAt
	//     f.Run.CreatedAt; then f.Run is inserted as in 4;
	//  4. otherwise f.Run is inserted. A pending f.Run while the schedule
	//     has a pending run violates the one-pending-run rule:
	//     ErrAlreadyExists, and nothing changes.
	//
	// It returns the run that holds the fire (f.Run, or the merged pending
	// run, whose ID differs). ErrNotFound if the schedule does not exist.
	FireSchedule(ctx context.Context, f Fire) (Run, error)

	// AddRun adds a run outside the cron (RunNow): if merge and the
	// schedule has a pending run, that run absorbs r (Coalesced +
	// r.Coalesced + 1, Deadline = max(Deadline, r.Deadline): a manual
	// request may wait as long as its own policy allows) and is returned;
	// otherwise r is inserted and returned. The schedule's fire state is
	// untouched. ErrNotFound if the schedule does not exist;
	// ErrAlreadyExists if r.ID is taken or r is pending while another run
	// is.
	AddRun(ctx context.Context, r Run, merge bool) (Run, error)

	// GetRun returns a run. ErrNotFound if missing.
	GetRun(ctx context.Context, id domain.ScheduleRunID) (Run, error)

	// ListOpenRuns returns at most limit pending and launching runs (of
	// every schedule), by ScheduledFor, then ID (index
	// schedule_runs_open_idx).
	ListOpenRuns(ctx context.Context, limit int) ([]Run, error)

	// UpdateRun is a compare-and-swap of a run's status: if the run's
	// Status is u.From it writes Status = u.To, Reason, Message, WorkerID
	// and JobID (when not zero) and UpdatedAt = u.At, and with
	// u.PauseSchedule disables the schedule as RunUpdate describes, in the
	// same transaction; it returns the stored run. ErrConflict if the
	// status differs (another tick or controller moved it), ErrNotFound if
	// missing, ErrAlreadyExists if u.To is pending and another run of the
	// schedule is, ErrInvalidArgument if a launching or launched run would
	// have no JobID.
	UpdateRun(ctx context.Context, u RunUpdate) (Run, error)

	// ListRuns returns at most f.Limit (negative: no limit) runs matching
	// every set field of f, by ID descending (newest first).
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)

	// CountRuns counts the runs matching f (f.Before and f.Limit are
	// ignored).
	CountRuns(ctx context.Context, f RunFilter) (int64, error)

	// LatestRuns returns the newest run (greatest ID) of each of ids that
	// has one.
	LatestRuns(ctx context.Context, ids []domain.ScheduleID) (map[domain.ScheduleID]Run, error)

	// HasActiveRun reports whether the schedule has a launching run, or a
	// job (jobs.Job.ScheduleID) that is queued, dispatched or running
	// (index jobs_schedule_active_idx).
	HasActiveRun(ctx context.Context, id domain.ScheduleID) (bool, error)

	// ListUpcoming returns the next occurrences of at most limit enabled
	// schedules of owner (zero: every owner) with a NextRunAt, by
	// NextRunAt, then ID (index schedules_owner_next_idx).
	ListUpcoming(ctx context.Context, owner domain.UserID, limit int) ([]Upcoming, error)
}

// RemoteChecker checks the remotes of a schedule's definition
// (*remotes.Service).
type RemoteChecker interface {
	// Usable returns the names of ids, which must all be live remotes of
	// owner: ErrNotFound if any is missing, deleted or another user's.
	Usable(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error)
}

// ContactChecker checks the contacts linked to a schedule
// (*contacts.Service).
type ContactChecker interface {
	// Usable returns the names of ids, which must all be contacts of
	// owner: ErrNotFound if any is missing or another user's.
	Usable(ctx context.Context, owner domain.UserID, ids []domain.ContactID) (map[domain.ContactID]string, error)
}

// WorkerChecker checks a schedule's target worker. The composition root
// adapts *workers.Service (its Get returns ErrNotFound for a missing or
// removed worker).
type WorkerChecker interface {
	// CheckWorker returns nil if the worker exists and is not removed, and
	// an error wrapping ErrNotFound otherwise.
	CheckWorker(ctx context.Context, id domain.WorkerID) error
}

// Waker asks the scheduler to run a pass soon (after a schedule was saved
// or RunNow added a run). It must not block.
type Waker interface {
	Wake()
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints schedule and run IDs. They must be time-ordered
// (UUIDv7) for the newest-first run lists.
type IDGenerator interface {
	NewScheduleID() domain.ScheduleID
	NewScheduleRunID() domain.ScheduleRunID
}
