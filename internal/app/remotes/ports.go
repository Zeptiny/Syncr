package remotes

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists remotes with their union members. Implementations
// must be safe for concurrent use; every method is atomic on its own (a
// remote and its member rows change in one transaction). Errors wrap the
// domain sentinels named below; anything else is an internal error. Returned
// values must not alias storage.
type Repository interface {
	// CreateRemote inserts r with its members (Version is stored as given,
	// normally 1). ErrAlreadyExists if r.ID is taken or the owner has a
	// live remote with the same name, case-insensitively; ErrNotFound if
	// the owner or a member remote does not exist, or a member is deleted.
	CreateRemote(ctx context.Context, r Remote) error

	// GetRemote returns a remote with its members, deleted or not.
	// ErrNotFound if it never existed.
	GetRemote(ctx context.Context, id domain.RemoteID) (Remote, error)

	// GetRemotes returns the remotes of ids that exist (deleted ones
	// included) with their members, by ID; missing ones are omitted and
	// duplicate IDs are returned once.
	GetRemotes(ctx context.Context, ids []domain.RemoteID) ([]Remote, error)

	// ListRemotes returns the live remotes matching f with their members,
	// by lower-cased name, then ID.
	ListRemotes(ctx context.Context, f ListFilter) ([]Remote, error)

	// UpdateRemote is an optimistic, version-checked update of a live
	// remote: if the stored remote's Version equals r.Version, it writes
	// Name, Options, SecretsEnc, SecretKeys, Members and UpdatedAt, sets
	// Version = r.Version + 1 and returns the stored remote. ID, OwnerID,
	// Type, CreatedAt and DeletedAt are never written. ErrConflict if the
	// version differs; ErrNotFound if missing or deleted, or if a member
	// remote does not exist or is deleted; ErrAlreadyExists if the new
	// name is taken.
	UpdateRemote(ctx context.Context, r Remote) (Remote, error)

	// DeleteRemote soft-deletes a live remote: it sets DeletedAt to at,
	// clears SecretsEnc and increments Version; the remote's own members
	// are kept. ErrNotFound if missing or already deleted;
	// ErrFailedPrecondition (naming the union) if it is a member of a live
	// union, checked in the same transaction.
	DeleteRemote(ctx context.Context, id domain.RemoteID, at time.Time) error

	// ListUnionsUsing returns the live unions that have member among their
	// members, by lower-cased name, then ID.
	ListUnionsUsing(ctx context.Context, member domain.RemoteID) ([]Remote, error)
}

// Cipher encrypts secrets at rest; associated data binds a ciphertext to its
// record. internal/adapters/secrets implements it.
type Cipher interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints remote IDs (UUIDv7).
type IDGenerator interface {
	NewRemoteID() domain.RemoteID
}
