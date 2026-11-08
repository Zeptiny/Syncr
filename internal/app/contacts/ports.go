package contacts

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists contacts. Implementations must be safe for concurrent
// use; every method is atomic on its own. Errors wrap the domain sentinels
// named below; anything else is an internal error. Returned values must not
// alias storage.
type Repository interface {
	// CreateContact inserts c (Version is stored as given, normally 1).
	// ErrAlreadyExists if c.ID is taken or the owner has a contact with the
	// same name, case-insensitively; ErrNotFound if the owner does not
	// exist; ErrInvalidArgument if JobEvents is not a subset of
	// domain.JobEvents (a check).
	CreateContact(ctx context.Context, c Contact) error

	// GetContact returns a contact. ErrNotFound if missing.
	GetContact(ctx context.Context, id domain.ContactID) (Contact, error)

	// GetContacts returns the contacts of ids that exist, by ID; missing
	// ones are omitted and duplicate IDs are returned once.
	GetContacts(ctx context.Context, ids []domain.ContactID) ([]Contact, error)

	// ListContacts returns the contacts matching f, by lower-cased name,
	// then ID.
	ListContacts(ctx context.Context, f ListFilter) ([]Contact, error)

	// UpdateContact is an optimistic, version-checked update: if the stored
	// contact's Version equals c.Version, it writes Name, Options,
	// SecretsEnc, SecretKeys, Summary, Enabled, JobEvents and UpdatedAt,
	// sets Version = c.Version + 1 and returns the stored contact. ID,
	// OwnerID, Type and CreatedAt are never written. ErrConflict if the
	// version differs, ErrNotFound if missing, ErrAlreadyExists if the new
	// name is taken, ErrInvalidArgument as for CreateContact.
	UpdateContact(ctx context.Context, c Contact) (Contact, error)

	// DeleteContact deletes a contact with its schedule links and its
	// deliveries (a hard delete). ErrNotFound if missing.
	DeleteContact(ctx context.Context, id domain.ContactID) error

	// ContactSchedules returns, for each of ids linked to schedules, the
	// schedules linked to it, by lower-cased name, then ID
	// (Contact.used_by).
	ContactSchedules(ctx context.Context, ids []domain.ContactID) (map[domain.ContactID][]ScheduleRef, error)
}

// Cipher encrypts secrets at rest; associated data binds a ciphertext to its
// record. internal/adapters/secrets implements it.
type Cipher interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
}

// Limiter is a keyed rate limiter (platform/ratelimit): Allow consumes one
// token of key's bucket and reports whether one was available. SendTest is
// limited per user.
type Limiter interface {
	Allow(key string) bool
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints contact IDs (UUIDv7), and the IDs of test
// notifications (the delivery kind, though they are never stored: receivers
// see it as the message ID, e.g. the webhook-id header).
type IDGenerator interface {
	NewContactID() domain.ContactID
	NewDeliveryID() domain.DeliveryID
}
