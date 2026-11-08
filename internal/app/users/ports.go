package users

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists user accounts. Implementations must be safe for
// concurrent use; every method is atomic on its own. Errors wrap the domain
// sentinels named below; anything else is an internal error. Returned values
// must not alias storage.
type Repository interface {
	// CreateUser inserts u (Version is stored as given, normally 1).
	// ErrAlreadyExists if u.ID or u.Username is taken.
	CreateUser(ctx context.Context, u User) error

	// CreateFirstUser inserts u only if no user exists, atomically: of
	// concurrent calls on an empty store exactly one succeeds, the others
	// get ErrFailedPrecondition (as does any call once a user exists).
	// Postgres: pg_advisory_xact_lock, then a count, then the insert, in
	// one transaction.
	CreateFirstUser(ctx context.Context, u User) error

	// CountUsers returns the number of users (disabled ones included).
	CountUsers(ctx context.Context) (int, error)

	// GetUser returns a user. ErrNotFound if missing.
	GetUser(ctx context.Context, id domain.UserID) (User, error)

	// GetUserByUsername returns the user with exactly this username (the
	// caller lower-cases it). ErrNotFound if missing.
	GetUserByUsername(ctx context.Context, username string) (User, error)

	// ListUsers returns every user, by username.
	ListUsers(ctx context.Context) ([]User, error)

	// UpdateUser is an optimistic, version-checked update: if the stored
	// user's Version equals u.Version, it writes DisplayName, Role,
	// PasswordHash, DisabledAt, PasswordChangedAt, LastLoginAt and
	// UpdatedAt, sets Version = u.Version + 1 and returns the stored user.
	// ID, Username and CreatedAt are never written. ErrConflict if the
	// version differs, ErrNotFound if missing.
	UpdateUser(ctx context.Context, u User) (User, error)

	// CountActiveAdmins returns the number of admins that are not disabled.
	CountActiveAdmins(ctx context.Context) (int, error)

	// DeleteUser deletes a user with their sessions and API tokens.
	// ErrNotFound if missing; ErrFailedPrecondition if the user owns remotes
	// (deleted ones included), jobs, schedules or contacts (Postgres: the
	// foreign keys).
	DeleteUser(ctx context.Context, id domain.UserID) error
}

// SessionRepository persists sessions. Same rules as Repository.
type SessionRepository interface {
	// CreateSession inserts s. ErrAlreadyExists if s.IDHash is taken;
	// ErrNotFound if s.UserID does not exist.
	CreateSession(ctx context.Context, s Session) error

	// GetSession returns the session with hash h, expired or not.
	// ErrNotFound if missing.
	GetSession(ctx context.Context, h TokenHash) (Session, error)

	// TouchSession sets the session's LastSeenAt to at and IdleExpiresAt to
	// idleExpiresAt. ErrNotFound if missing.
	TouchSession(ctx context.Context, h TokenHash, at, idleExpiresAt time.Time) error

	// DeleteSession deletes the session with hash h; deleting a missing
	// session is not an error.
	DeleteSession(ctx context.Context, h TokenHash) error

	// DeleteUserSessions deletes every session of user except keep (nil
	// keeps none) and returns how many it deleted.
	DeleteUserSessions(ctx context.Context, user domain.UserID, keep *TokenHash) (int, error)

	// DeleteExpiredSessions deletes the sessions that are expired at now
	// (Session.Expired) and returns how many it deleted.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}

// APITokenRepository persists personal API tokens. Same rules as
// Repository.
type APITokenRepository interface {
	// CreateAPIToken inserts t. ErrAlreadyExists if t.ID or t.Hash is taken;
	// ErrNotFound if t.UserID does not exist.
	CreateAPIToken(ctx context.Context, t APIToken) error

	// GetAPITokenByHash returns the token with hash h, expired or not.
	// ErrNotFound if missing.
	GetAPITokenByHash(ctx context.Context, h TokenHash) (APIToken, error)

	// ListAPITokens returns the user's tokens by ID descending (newest
	// first).
	ListAPITokens(ctx context.Context, user domain.UserID) ([]APIToken, error)

	// DeleteAPIToken deletes the user's token id. ErrNotFound if missing or
	// owned by another user.
	DeleteAPIToken(ctx context.Context, user domain.UserID, id domain.APITokenID) error

	// TouchAPIToken sets the token's LastUsedAt to at. ErrNotFound if
	// missing.
	TouchAPIToken(ctx context.Context, id domain.APITokenID, at time.Time) error

	// DeleteExpiredAPITokens deletes the tokens that are expired at now
	// (APIToken.Expired) and returns how many it deleted.
	DeleteExpiredAPITokens(ctx context.Context, now time.Time) (int, error)

	// DeleteUserAPITokens deletes every token of user (when the user is
	// disabled, so re-enabling does not revive them) and returns how many it
	// deleted.
	DeleteUserAPITokens(ctx context.Context, user domain.UserID) (int, error)
}

// PasswordHasher hashes and verifies passwords (argon2id PHC strings;
// internal/adapters/password implements it). Implementations bound their
// concurrency and honor ctx while waiting.
type PasswordHasher interface {
	// Hash returns the encoded hash of password.
	Hash(ctx context.Context, password string) (string, error)
	// Verify reports whether password matches encoded, in constant time,
	// and whether encoded uses outdated parameters and should be replaced
	// by a new Hash. A malformed encoded hash is an error.
	Verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error)
}

// Limiter is a keyed rate limiter (a token bucket per key). Allow consumes
// one token of key's bucket and reports whether one was available.
// Implementations must be safe for concurrent use and bound their memory.
type Limiter interface {
	Allow(key string) bool
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints IDs. They must be time-ordered (UUIDv7).
type IDGenerator interface {
	NewUserID() domain.UserID
	NewAPITokenID() domain.APITokenID
}
