package domain

import (
	"fmt"
	"uuid"
)

// ID is a UUID identifying an entity of kind K. K is a phantom type parameter:
// it makes IDs of different entities distinct types, so a JobID cannot be
// passed where a WorkerID is expected. Use the aliases below (JobID,
// WorkerID, ...) rather than instantiating ID directly.
//
// New IDs are UUIDv7 (time-ordered, which keeps database indexes compact).
// The zero value is the nil UUID and means "no ID".
type ID[K any] struct {
	uuid uuid.UUID
}

// Phantom kinds; unexported so IDs can only be minted through this package.
type (
	userKind       struct{}
	workerKind     struct{}
	remoteKind     struct{}
	resticRepoKind struct{}
	scheduleKind   struct{}
	jobKind        struct{}
	apiTokenKind   struct{}
	contactKind    struct{}
	runKind        struct{}
	deliveryKind   struct{}
)

// Entity IDs.
type (
	// UserID identifies a user account.
	UserID = ID[userKind]
	// WorkerID identifies an enrolled worker.
	WorkerID = ID[workerKind]
	// RemoteID identifies a configured remote (storage backend).
	RemoteID = ID[remoteKind]
	// ResticRepoID identifies a restic repository definition.
	ResticRepoID = ID[resticRepoKind]
	// ScheduleID identifies a schedule.
	ScheduleID = ID[scheduleKind]
	// JobID identifies a job run.
	JobID = ID[jobKind]
	// APITokenID identifies a user's personal API token.
	APITokenID = ID[apiTokenKind]
	// ContactID identifies a notification contact.
	ContactID = ID[contactKind]
	// ScheduleRunID identifies one run of a schedule.
	ScheduleRunID = ID[runKind]
	// DeliveryID identifies one notification to one contact.
	DeliveryID = ID[deliveryKind]
)

// NewUserID returns a new random UserID.
func NewUserID() UserID { return newID[userKind]() }

// NewWorkerID returns a new random WorkerID.
func NewWorkerID() WorkerID { return newID[workerKind]() }

// NewRemoteID returns a new random RemoteID.
func NewRemoteID() RemoteID { return newID[remoteKind]() }

// NewResticRepoID returns a new random ResticRepoID.
func NewResticRepoID() ResticRepoID { return newID[resticRepoKind]() }

// NewScheduleID returns a new random ScheduleID.
func NewScheduleID() ScheduleID { return newID[scheduleKind]() }

// NewJobID returns a new random JobID.
func NewJobID() JobID { return newID[jobKind]() }

// NewAPITokenID returns a new random APITokenID.
func NewAPITokenID() APITokenID { return newID[apiTokenKind]() }

// NewContactID returns a new random ContactID.
func NewContactID() ContactID { return newID[contactKind]() }

// NewScheduleRunID returns a new random ScheduleRunID.
func NewScheduleRunID() ScheduleRunID { return newID[runKind]() }

// NewDeliveryID returns a new random DeliveryID.
func NewDeliveryID() DeliveryID { return newID[deliveryKind]() }

// ParseUserID parses a UserID from its canonical string form.
func ParseUserID(s string) (UserID, error) { return parseID[userKind](s) }

// ParseWorkerID parses a WorkerID from its canonical string form.
func ParseWorkerID(s string) (WorkerID, error) { return parseID[workerKind](s) }

// ParseRemoteID parses a RemoteID from its canonical string form.
func ParseRemoteID(s string) (RemoteID, error) { return parseID[remoteKind](s) }

// ParseResticRepoID parses a ResticRepoID from its canonical string form.
func ParseResticRepoID(s string) (ResticRepoID, error) { return parseID[resticRepoKind](s) }

// ParseScheduleID parses a ScheduleID from its canonical string form.
func ParseScheduleID(s string) (ScheduleID, error) { return parseID[scheduleKind](s) }

// ParseJobID parses a JobID from its canonical string form.
func ParseJobID(s string) (JobID, error) { return parseID[jobKind](s) }

// ParseAPITokenID parses an APITokenID from its canonical string form.
func ParseAPITokenID(s string) (APITokenID, error) { return parseID[apiTokenKind](s) }

// ParseContactID parses a ContactID from its canonical string form.
func ParseContactID(s string) (ContactID, error) { return parseID[contactKind](s) }

// ParseScheduleRunID parses a ScheduleRunID from its canonical string form.
func ParseScheduleRunID(s string) (ScheduleRunID, error) { return parseID[runKind](s) }

// ParseDeliveryID parses a DeliveryID from its canonical string form.
func ParseDeliveryID(s string) (DeliveryID, error) { return parseID[deliveryKind](s) }

func newID[K any]() ID[K] { return ID[K]{uuid: uuid.NewV7()} }

// parseID parses a non-nil UUID. Errors wrap ErrInvalidID.
func parseID[K any](s string) (ID[K], error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID[K]{}, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	if u == uuid.Nil() {
		return ID[K]{}, fmt.Errorf("%w: nil UUID", ErrInvalidID)
	}
	return ID[K]{uuid: u}, nil
}

// IsZero reports whether id is the zero (nil) ID.
func (id ID[K]) IsZero() bool { return id.uuid == uuid.Nil() }

// UUID returns the underlying UUID, for adapters that store or transmit it.
func (id ID[K]) UUID() uuid.UUID { return id.uuid }

// String returns the canonical lower-case UUID form.
func (id ID[K]) String() string { return id.uuid.String() }

// MarshalText implements encoding.TextMarshaler.
func (id ID[K]) MarshalText() ([]byte, error) { return id.uuid.MarshalText() }

// UnmarshalText implements encoding.TextUnmarshaler. Unlike the Parse
// functions it accepts the nil UUID, so zero IDs round-trip.
func (id *ID[K]) UnmarshalText(b []byte) error {
	var u uuid.UUID
	if err := u.UnmarshalText(b); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidID, b)
	}
	id.uuid = u
	return nil
}
