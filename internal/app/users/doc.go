// Package users owns user accounts, first-run setup, sessions and personal
// API tokens (Milestone 3).
//
// # Model
//
// Syncr is self-hosted for trusted users: there is no public sign-up. The
// first admin is created by the setup wizard, gated by a one-time setup code
// the controller logs at start while no user exists; admins create every
// other account. A user has a role (domain.RoleAdmin or domain.RoleUser);
// the API's policy table decides which procedures a role may call, and the
// use cases scope every object by the calling domain.Actor.
//
// Passwords are stored as argon2id PHC strings (PasswordHasher). Session
// tokens and API tokens are random secrets of which only the SHA-256 hash
// (TokenHash) is stored; the raw session token lives only in the browser's
// cookie, the raw API token only with its owner.
//
// # Service (Milestone 3, track A)
//
// The Service is constructed as New(Deps, Config); Deps.Limiters holds the
// rate limiters of the credential checks. NewSetupCode returns a random
// setup code. Its methods:
//
//	// SetupRequired reports whether no user exists yet.
//	SetupRequired(ctx) (bool, error)
//	// ArmSetup sets the one-time setup code (held only as a hash; at least
//	// MinSetupCodeLen characters); the composition root calls it at start
//	// while no user exists.
//	ArmSetup(code string) error
//	// CompleteSetup checks the code (constant time, rate limited) and
//	// creates the first admin with Repository.CreateFirstUser, then starts
//	// a session. ErrFailedPrecondition once a user exists.
//	CompleteSetup(ctx, SetupParams) (User, IssuedSession, error)
//	// Login checks the credentials (rate limited per client IP and per
//	// username, unknown usernames included) and starts a session.
//	// ErrUnauthenticated for wrong credentials or a disabled account;
//	// ErrRateLimited when a limit is exceeded.
//	Login(ctx, LoginParams) (User, IssuedSession, error)
//	// Logout ends the session of a raw session token.
//	Logout(ctx, sessionToken string) error
//	// AuthenticateSession resolves a raw session token to its actor,
//	// extending its idle expiry (stored at most once a minute).
//	// ErrUnauthenticated if unknown, expired or the user is disabled.
//	AuthenticateSession(ctx, token string) (domain.Actor, Session, error)
//	// AuthenticateAPIToken resolves a raw API token to its owner's actor
//	// (the token is returned for its expiry); its last use is stored at
//	// most once a minute.
//	AuthenticateAPIToken(ctx, token string) (domain.Actor, APIToken, error)
//	// Me returns the actor's own account.
//	Me(ctx, actor domain.Actor) (User, error)
//	// ChangePassword checks the current password, stores the new one and
//	// revokes the actor's other sessions (all but sessionToken's).
//	ChangePassword(ctx, actor domain.Actor, sessionToken, current, next string) error
//	// CreateAPIToken mints a token for the actor; the raw secret is
//	// returned once. ttl 0 means no expiry.
//	CreateAPIToken(ctx, actor domain.Actor, name string, ttl time.Duration) (APIToken, string, error)
//	ListAPITokens(ctx, actor domain.Actor) ([]APIToken, error)
//	DeleteAPIToken(ctx, actor domain.Actor, id domain.APITokenID) error
//
//	// Admin use cases: ErrPermissionDenied unless actor.IsAdmin() (the API
//	// checks the role too; this is defense in depth). Disabling, demoting
//	// or deleting the last active admin is ErrFailedPrecondition; deleting
//	// a user who owns remotes or jobs is ErrFailedPrecondition. Password
//	// resets, disabling and role changes revoke the user's sessions.
//	List(ctx, actor domain.Actor) ([]User, error)
//	Get(ctx, actor domain.Actor, id domain.UserID) (User, error)
//	Create(ctx, actor domain.Actor, p CreateParams) (User, error)
//	Update(ctx, actor domain.Actor, id domain.UserID, p UpdateParams) (User, error)
//	ResetPassword(ctx, actor domain.Actor, id domain.UserID, password string) (User, error)
//	Delete(ctx, actor domain.Actor, id domain.UserID) error
//
//	// PurgeExpired deletes expired sessions (and any left of disabled
//	// users); the composition root calls it periodically.
//	PurgeExpired(ctx) (int, error)
//
// Usernames are lower case and match ^[a-z0-9][a-z0-9._-]{0,63}$ (the
// store enforces it too); passwords are 12 to 256 characters with no
// composition rules. Invalid input is reported as *domain.ValidationError.
//
// Allowed dependencies: the standard library and internal/domain; every
// other collaborator is a port declared in ports.go.
package users
