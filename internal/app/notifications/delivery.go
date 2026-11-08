package notifications

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Status is where a delivery is in the outbox. The string values are
// stable: they are stored.
type Status string

// Delivery statuses.
const (
	// StatusPending waits for its next attempt (NextAttemptAt).
	StatusPending Status = "pending"
	// StatusSending is claimed by a deliverer until LeaseUntil.
	StatusSending Status = "sending"
	// StatusDelivered was accepted by the channel. Terminal.
	StatusDelivered Status = "delivered"
	// StatusDead failed permanently or ran out of attempts. Terminal, but
	// Retry queues it again.
	StatusDead Status = "dead"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	return s == StatusPending || s == StatusSending || s == StatusDelivered || s == StatusDead
}

// Delivery is one notification to one contact, stored before it is sent
// (the outbox).
type Delivery struct {
	ID        domain.DeliveryID
	ContactID domain.ContactID
	// OwnerID is the contact's owner.
	OwnerID domain.UserID
	Event   domain.NotificationEvent
	// DedupeKey is unique per contact: JobDedupeKey or RunDedupeKey, so
	// planning an outcome twice inserts nothing the second time.
	DedupeKey string
	// JobID is the reported job; zero for a run outcome. ScheduleID is the
	// schedule, if any (cleared when the schedule is deleted).
	JobID      domain.JobID
	ScheduleID domain.ScheduleID
	// Payload is the JSON of the notify.Message to send, with an empty ID
	// (set to the delivery ID at send time). It contains no secrets.
	Payload []byte

	Status Status
	// Attempts counts claims (an expired lease counts too); MaxAttempts is
	// the limit fixed when the delivery was planned.
	Attempts    int
	MaxAttempts int
	// NextAttemptAt is when a pending delivery is due. LeaseUntil is when
	// a sending delivery's claim expires (zero otherwise). LastAttemptAt is
	// when the last claim started; DeliveredAt when the channel accepted it.
	NextAttemptAt time.Time
	LeaseUntil    time.Time
	LastAttemptAt time.Time
	DeliveredAt   time.Time
	// LastError is the sanitized reason of the last failed attempt
	// (notify.SendError.Reason); never contains secrets.
	LastError string
	CreatedAt time.Time
}

// JobDedupeKey is the dedupe key of a job outcome's deliveries.
func JobDedupeKey(id domain.JobID, e domain.NotificationEvent) string {
	return "job:" + id.String() + ":" + string(e)
}

// RunDedupeKey is the dedupe key of a run outcome's deliveries.
func RunDedupeKey(id domain.ScheduleRunID, e domain.NotificationEvent) string {
	return "run:" + id.String() + ":" + string(e)
}

// Subscriber is a contact that may be notified of an outcome, with the
// events it subscribed to. The planner notifies the subscribers whose
// Events contain the outcome's event.
type Subscriber struct {
	ContactID domain.ContactID
	Events    domain.EventSet
}

// JobOutcome is a finished job that was not notified yet (planner input),
// with what its message needs and its subscribers.
type JobOutcome struct {
	JobID   domain.JobID
	OwnerID domain.UserID
	Kind    string
	// Definition is the job's stored definition (jobs.Job.Definition), for
	// its description (JobDescriber).
	Definition []byte
	Status     domain.JobStatus
	WorkerID   domain.WorkerID
	WorkerName string
	// ScheduleID and ScheduleName are set for a job of a schedule that
	// still exists.
	ScheduleID   domain.ScheduleID
	ScheduleName string
	ErrorCode    string
	ErrorMessage string
	Bytes        int64
	Files        int64
	Errors       int64
	StartedAt    time.Time
	FinishedAt   time.Time
	// Subscribers are, for a scheduled job, the enabled contacts linked to
	// its schedule with the links' events; for a manual job, the owner's
	// enabled contacts with non-empty JobEvents, with those events. By
	// contact ID.
	Subscribers []Subscriber
}

// RunOutcome is a missed or failed run that was not notified yet (planner
// input).
type RunOutcome struct {
	RunID        domain.ScheduleRunID
	ScheduleID   domain.ScheduleID
	ScheduleName string
	OwnerID      domain.UserID
	// Status is the run's status, "missed" or "failed" (schedules.RunStatus
	// values).
	Status       string
	Reason       string
	Message      string
	ScheduledFor time.Time
	Coalesced    int
	WorkerID     domain.WorkerID
	// UpdatedAt is when the run ended.
	UpdatedAt time.Time
	// Subscribers are the enabled contacts linked to the schedule, with the
	// links' events, by contact ID.
	Subscribers []Subscriber
}

// Completion is the outcome of one claimed attempt
// (Repository.CompleteDelivery).
type Completion struct {
	ID domain.DeliveryID
	// Attempts is the delivery's Attempts as claimed: the completion is a
	// compare-and-swap on it, so a deliverer whose lease expired and was
	// reclaimed cannot overwrite the newer attempt.
	Attempts int
	// Status is delivered, pending (retry at NextAttemptAt) or dead.
	Status        Status
	NextAttemptAt time.Time
	LastError     string
	// At is the completion time (DeliveredAt for a delivered one).
	At time.Time
}

// DeliveryFilter selects deliveries. Zero fields do not filter.
type DeliveryFilter struct {
	// OwnerID keeps only deliveries of this user's contacts (the service
	// always sets it to the actor).
	OwnerID   domain.UserID
	ContactID domain.ContactID
	Statuses  []Status
	// Before is a keyset cursor: only deliveries with ID < Before.
	Before domain.DeliveryID
	// Limit is the number to return; negative means no limit
	// (repositories), 0 means DefaultPageSize (the service).
	Limit int
}

// Page sizes of delivery lists.
const (
	DefaultPageSize = 50
	MaxPageSize     = 500
)

// Page is one page of deliveries, newest first.
type Page struct {
	Deliveries []Delivery
	// Next is the cursor (DeliveryFilter.Before) of the next page; zero on
	// the last page.
	Next domain.DeliveryID
}

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// MaxAttempts is the attempt limit of new deliveries (notify.max_attempts).
	MaxAttempts int
	// Concurrency is the number of deliveries sent at once (notify.concurrency).
	Concurrency int
	// Lease is how long a claim lasts before another deliverer may reclaim it.
	Lease time.Duration
	// Tick is the period of the planner, deliverer and purge loop (the
	// backstop of Wake).
	Tick time.Duration
	// Retention is how long delivered and dead deliveries are kept
	// (notify.retention).
	Retention time.Duration
	// SendTimeout bounds one Send (notify.send_timeout).
	SendTimeout time.Duration
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		MaxAttempts: 8, Concurrency: 4, Lease: 2 * time.Minute, Tick: 30 * time.Second,
		Retention: 30 * 24 * time.Hour, SendTimeout: 15 * time.Second,
	}
}
