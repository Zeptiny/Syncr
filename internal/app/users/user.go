package users

import (
	"crypto/sha256"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// User is a user account.
type User struct {
	ID domain.UserID
	// Username is the login name: lower case, ^[a-z0-9][a-z0-9._-]{0,63}$,
	// unique. It never changes.
	Username string
	// DisplayName is the name to display; may be empty.
	DisplayName string
	Role        domain.Role
	// PasswordHash is the argon2id PHC string of the password. It never
	// leaves the service.
	PasswordHash string
	// DisabledAt is when the account was disabled; zero while enabled.
	DisabledAt time.Time

	CreatedAt         time.Time
	UpdatedAt         time.Time
	PasswordChangedAt time.Time
	// LastLoginAt is the last successful login; zero if never.
	LastLoginAt time.Time

	// Version increases with every stored change; it is the optimistic
	// concurrency token of Repository.UpdateUser. A created user has
	// version 1.
	Version int64
}

// Disabled reports whether the account is disabled.
func (u User) Disabled() bool { return !u.DisabledAt.IsZero() }

// Actor returns the actor acting as u.
func (u User) Actor() domain.Actor { return domain.Actor{UserID: u.ID, Role: u.Role} }

// TokenHash is the SHA-256 hash of a raw session or API token: the only form
// in which tokens are stored.
type TokenHash [sha256.Size]byte

// HashToken returns the hash of a raw token.
func HashToken(token string) TokenHash { return sha256.Sum256([]byte(token)) }

// Session is a logged-in browser session.
type Session struct {
	// IDHash is the hash of the raw session token (the cookie value).
	IDHash TokenHash
	UserID domain.UserID

	CreatedAt time.Time
	// LastSeenAt is the last authenticated request (stored at most once a
	// minute).
	LastSeenAt time.Time
	// IdleExpiresAt moves forward with use; ExpiresAt is the absolute end.
	// The session is valid while now is before both.
	IdleExpiresAt time.Time
	ExpiresAt     time.Time

	// UserAgent and ClientIP of the login, for the user's information.
	UserAgent string
	ClientIP  string
}

// Expired reports whether the session is no longer valid at now.
func (s Session) Expired(now time.Time) bool {
	return !now.Before(s.IdleExpiresAt) || !now.Before(s.ExpiresAt)
}

// APIToken is a personal API token. It acts with its owner's role.
type APIToken struct {
	ID     domain.APITokenID
	UserID domain.UserID
	// Name is the owner's label for the token.
	Name string
	// Prefix is the start of the raw token ("syncr_pat_" plus four
	// characters), shown to recognize it.
	Prefix string
	// Hash is the hash of the raw token; unique.
	Hash TokenHash

	CreatedAt time.Time
	// ExpiresAt is zero if the token never expires.
	ExpiresAt time.Time
	// LastUsedAt is zero if never used (stored at most once a minute).
	LastUsedAt time.Time
}

// Expired reports whether the token is no longer valid at now.
func (t APIToken) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// IssuedSession is a new session as the API hands it to the browser. Token
// is the raw session token: it goes only into the cookie.
type IssuedSession struct {
	Token     string
	ExpiresAt time.Time
}

// SetupParams are the inputs of CompleteSetup.
type SetupParams struct {
	// Code is the setup code as typed (dashes and case are ignored).
	Code        string
	Username    string
	DisplayName string
	Password    string
	// ClientIP and UserAgent of the request, for rate limiting and the
	// session record.
	ClientIP  string
	UserAgent string
}

// LoginParams are the inputs of Login.
type LoginParams struct {
	Username  string
	Password  string
	ClientIP  string
	UserAgent string
}

// CreateParams are the inputs of the admin use case Create.
type CreateParams struct {
	Username    string
	DisplayName string
	Password    string
	Role        domain.Role
}

// UpdateParams are the inputs of the admin use case Update. Nil or zero
// fields keep the current value.
type UpdateParams struct {
	// Version is the version the caller read (ErrConflict if it changed).
	Version     int64
	DisplayName *string
	// Role "" keeps the current role.
	Role     domain.Role
	Disabled *bool
}
