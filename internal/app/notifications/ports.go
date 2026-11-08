package notifications

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository is the notification outbox. Implementations must be safe for
// concurrent use; every method is atomic on its own (Commit* and Claim are
// one transaction or one statement each). Errors wrap the domain sentinels
// named below; anything else is an internal error. Returned values must not
// alias storage.
type Repository interface {
	// PendingJobOutcomes returns at most limit terminal jobs (succeeded,
	// failed, cancelled, interrupted) that were not notified yet, by
	// FinishedAt, then ID, with their definition, joined with their
	// worker's and schedule's names and their subscribers (JobOutcome). Cancelled jobs are returned too,
	// so the planner marks them notified without deliveries.
	PendingJobOutcomes(ctx context.Context, limit int) ([]JobOutcome, error)

	// PendingRunOutcomes returns at most limit missed and failed runs that
	// were not notified yet, by UpdatedAt, then ID, with their schedule's
	// name and subscribers (RunOutcome).
	PendingRunOutcomes(ctx context.Context, limit int) ([]RunOutcome, error)

	// CommitJobOutcome plans a job's notifications, in one transaction: it
	// inserts ds, skipping any whose (ContactID, DedupeKey) exists (ON
	// CONFLICT DO NOTHING) or whose contact no longer exists, then marks
	// the job notified at at (if it is not already). It returns how many
	// deliveries it inserted. Concurrent or repeated commits of one job
	// insert each delivery once. ErrNotFound if the job does not exist.
	CommitJobOutcome(ctx context.Context, id domain.JobID, ds []Delivery, at time.Time) (int, error)

	// CommitRunOutcome is CommitJobOutcome for a run.
	CommitRunOutcome(ctx context.Context, id domain.ScheduleRunID, ds []Delivery, at time.Time) (int, error)

	// ClaimDeliveries claims at most limit deliveries that are due, by
	// NextAttemptAt, then ID: pending ones with NextAttemptAt <= now, and
	// sending ones whose LeaseUntil <= now (an abandoned attempt). Each is
	// set to sending with Attempts + 1, LeaseUntil = now + lease and
	// LastAttemptAt = now, and returned as stored. Concurrent claims never
	// return the same delivery (Postgres: FOR UPDATE SKIP LOCKED).
	// Attempts may exceed MaxAttempts when the last attempt's lease
	// expired; the service then marks the delivery dead without sending.
	ClaimDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Delivery, error)

	// CompleteDelivery records the outcome of a claimed attempt if the
	// delivery is still sending with Attempts == c.Attempts: Status =
	// c.Status, LastError = c.LastError, LeaseUntil cleared, and
	// DeliveredAt = c.At (delivered) or NextAttemptAt = c.NextAttemptAt
	// (pending). It returns the stored delivery. ErrConflict if the claim
	// is stale (reclaimed or completed), ErrNotFound if missing (its
	// contact was deleted).
	CompleteDelivery(ctx context.Context, c Completion) (Delivery, error)

	// GetDelivery returns a delivery. ErrNotFound if missing.
	GetDelivery(ctx context.Context, id domain.DeliveryID) (Delivery, error)

	// ListDeliveries returns at most f.Limit (negative: no limit)
	// deliveries matching every set field of f, by ID descending (newest
	// first).
	ListDeliveries(ctx context.Context, f DeliveryFilter) ([]Delivery, error)

	// RetryDelivery queues a dead delivery again: Status pending, Attempts
	// 0, NextAttemptAt = at (LastError is kept). It returns the stored
	// delivery. ErrFailedPrecondition if it is not dead, ErrNotFound if
	// missing.
	RetryDelivery(ctx context.Context, id domain.DeliveryID, at time.Time) (Delivery, error)

	// PurgeDeliveries deletes the delivered and dead deliveries created
	// before before and returns how many it deleted.
	PurgeDeliveries(ctx context.Context, before time.Time) (int, error)
}

// ContactResolver resolves a delivery's contact at send time
// (*contacts.Service.ResolveChannel; app packages cannot share types, so it
// returns plain values).
type ContactResolver interface {
	// ResolveChannel returns the contact's channel type and decrypted
	// configuration. ErrNotFound if the contact was deleted;
	// ErrFailedPrecondition if its secrets cannot be decrypted.
	ResolveChannel(ctx context.Context, id domain.ContactID) (channelType string, cfg descriptor.Values, err error)
}

// JobDescriber describes jobs for their notifications (wired with the job
// kinds and the remotes service; app packages cannot use them directly).
type JobDescriber interface {
	// DescribeJob returns a one-line description of a job of owner from
	// its kind and stored definition, as the web UI's jobs list shows it,
	// e.g. "Copy Photos → B2 offsite". Remotes are named as the owner named
	// them, deleted ones included; only the owner's remotes are named. It
	// returns "" for a definition it cannot describe, and an error only
	// when the names cannot be read now (the outcome is planned again).
	DescribeJob(ctx context.Context, owner domain.UserID, kind string, definition []byte) (string, error)
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints delivery IDs (UUIDv7, for the newest-first lists).
type IDGenerator interface {
	NewDeliveryID() domain.DeliveryID
}
