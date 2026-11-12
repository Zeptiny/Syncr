package users

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Limits of the inputs.
const (
	// MinPasswordLen and MaxPasswordLen bound passwords, in characters
	// (NIST SP 800-63B: length, no composition rules).
	MinPasswordLen = 12
	MaxPasswordLen = 256
	// MaxUsernameLen is the longest username.
	MaxUsernameLen = 64
	// MaxDisplayNameLen is the longest display name, in characters.
	MaxDisplayNameLen = 128
	// MaxTokenNameLen is the longest API token name, in characters.
	MaxTokenNameLen = 64
	// MinAPITokenTTL and MaxAPITokenTTL bound the lifetime of an expiring
	// API token.
	MinAPITokenTTL = time.Minute
	MaxAPITokenTTL = 10 * 365 * 24 * time.Hour
)

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// SessionIdleTimeout ends a session that has not been used for this
	// long (auth.session_idle_timeout).
	SessionIdleTimeout time.Duration
	// SessionMaxAge ends a session this long after the login, however much
	// it is used (auth.session_max_age).
	SessionMaxAge time.Duration
	// TouchInterval is how often at most the last use of a session or API
	// token is stored.
	TouchInterval time.Duration
	// MaxAPITokens bounds the API tokens of one user.
	MaxAPITokens int
}

// DefaultConfig returns the default configuration: sessions end after 7
// days without use or 30 days after the login; uses are stored at most once
// a minute; a user may have 100 API tokens.
func DefaultConfig() Config {
	return Config{
		SessionIdleTimeout: 7 * 24 * time.Hour,
		SessionMaxAge:      30 * 24 * time.Hour,
		TouchInterval:      time.Minute,
		MaxAPITokens:       100,
	}
}

// Limiters are the rate limiters of the credential checks. Each is
// required. The values of the design are:
//
//	ClientIP: burst 10, then 1 every 30s, 10000 keys
//	Username: burst 5, then 1 every 60s, 10000 keys
//	Setup:    burst 5, then 1 every 60s (a single key)
type Limiters struct {
	// ClientIP limits Login and CompleteSetup attempts per client IP (one
	// budget for both).
	ClientIP Limiter
	// Username limits Login attempts per username, unknown usernames
	// included, and ChangePassword attempts per user (one budget for
	// both): password guessing costs the same through either.
	Username Limiter
	// Setup limits CompleteSetup attempts globally (one key).
	Setup Limiter
}

// Deps are the service's collaborators. All are required except Logger.
type Deps struct {
	Users    Repository
	Sessions SessionRepository
	Tokens   APITokenRepository
	Hasher   PasswordHasher
	Limiters Limiters
	Clock    Clock
	IDs      IDGenerator
	// Random is the source of session tokens, API tokens and setup codes:
	// crypto/rand.Reader in production.
	Random io.Reader
	// Logger defaults to discarding.
	Logger *slog.Logger
}

// Service implements the users use cases. It is safe for concurrent use.
//
// Its only state besides its dependencies is the armed setup code (a hash)
// and a lock serializing the admin changes that could remove the last
// active admin.
type Service struct {
	users    Repository
	sessions SessionRepository
	tokens   APITokenRepository
	hasher   PasswordHasher
	limits   Limiters
	clock    Clock
	ids      IDGenerator
	random   io.Reader
	log      *slog.Logger
	cfg      Config

	// dummyHash is verified against for unknown usernames, so that a login
	// for an unknown user costs as much as one with a wrong password.
	dummyHash string

	setupMu sync.Mutex
	// setupCode is the hash of the armed (normalized) setup code; nil while
	// none is armed.
	setupCode *TokenHash
	// setupDone caches that a user exists: it never goes back to none (the
	// last active admin cannot be deleted).
	setupDone atomic.Bool

	// adminMu serializes the changes that may leave no active admin (role
	// changes, disabling, deleting), so that the last-admin check and the
	// write cannot interleave with another such change in this process.
	adminMu sync.Mutex
}

// New returns a Service. It hashes a random password once (the dummy hash
// for unknown usernames), so it takes as long as one password hash.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Users == nil || d.Sessions == nil || d.Tokens == nil || d.Hasher == nil || d.Clock == nil || d.IDs == nil || d.Random == nil ||
		d.Limiters.ClientIP == nil || d.Limiters.Username == nil || d.Limiters.Setup == nil {
		return nil, errors.New("users: missing dependency")
	}
	def := DefaultConfig()
	cfg.SessionIdleTimeout = cmp.Or(cfg.SessionIdleTimeout, def.SessionIdleTimeout)
	cfg.SessionMaxAge = cmp.Or(cfg.SessionMaxAge, def.SessionMaxAge)
	cfg.TouchInterval = cmp.Or(cfg.TouchInterval, def.TouchInterval)
	cfg.MaxAPITokens = cmp.Or(cfg.MaxAPITokens, def.MaxAPITokens)
	if cfg.SessionIdleTimeout < time.Minute || cfg.SessionMaxAge < time.Minute || cfg.TouchInterval < 0 || cfg.MaxAPITokens < 1 {
		return nil, fmt.Errorf("users: invalid config: idle timeout %v and max age %v must be at least a minute, touch interval %v not negative, max api tokens %d positive",
			cfg.SessionIdleTimeout, cfg.SessionMaxAge, cfg.TouchInterval, cfg.MaxAPITokens)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		users: d.Users, sessions: d.Sessions, tokens: d.Tokens, hasher: d.Hasher, limits: d.Limiters,
		clock: d.Clock, ids: d.IDs, random: d.Random, log: log, cfg: cfg,
	}
	// The dummy password is random and never kept: nothing can match it.
	dummy, err := s.randomToken()
	if err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	if s.dummyHash, err = d.Hasher.Hash(context.Background(), dummy); err != nil {
		return nil, fmt.Errorf("users: hash the dummy password: %w", err)
	}
	return s, nil
}

// Me returns the actor's own account.
func (s *Service) Me(ctx context.Context, actor domain.Actor) (User, error) {
	if actor.UserID.IsZero() {
		return User{}, fmt.Errorf("get own account: %w", domain.ErrNotFound)
	}
	u, err := s.users.GetUser(ctx, actor.UserID)
	if err != nil {
		return User{}, fmt.Errorf("get own account: %w", err)
	}
	return u, nil
}

// List returns every user, by username. Admin only.
func (s *Service) List(ctx context.Context, actor domain.Actor) ([]User, error) {
	if err := requireAdmin(actor, "list users"); err != nil {
		return nil, err
	}
	us, err := s.users.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return us, nil
}

// Get returns a user. Admin only.
func (s *Service) Get(ctx context.Context, actor domain.Actor, id domain.UserID) (User, error) {
	if err := requireAdmin(actor, "get user"); err != nil {
		return User{}, err
	}
	if id.IsZero() {
		return User{}, fmt.Errorf("get user: %w", domain.ErrInvalidID)
	}
	u, err := s.users.GetUser(ctx, id)
	if err != nil {
		return User{}, fmt.Errorf("get user %s: %w", id, err)
	}
	return u, nil
}

// Create creates an account. Admin only. The username must already be in
// canonical (lower-case) form. ErrAlreadyExists if it is taken.
func (s *Service) Create(ctx context.Context, actor domain.Actor, p CreateParams) (User, error) {
	if err := requireAdmin(actor, "create user"); err != nil {
		return User{}, err
	}
	var v domain.Validation
	checkUsername(&v, "username", p.Username)
	displayName := checkDisplayName(&v, "display_name", p.DisplayName)
	checkPassword(&v, "password", p.Password)
	switch {
	case p.Role == "":
		v.Add("role", "is required")
	case !p.Role.Valid():
		v.Add("role", "is not a known role")
	}
	if err := v.Err(); err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	hash, err := s.hasher.Hash(ctx, p.Password)
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	now := s.clock.Now()
	u := User{
		ID: s.ids.NewUserID(), Username: p.Username, DisplayName: displayName, Role: p.Role, PasswordHash: hash,
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, Version: 1,
	}
	if err := s.users.CreateUser(ctx, u); err != nil {
		return User{}, fmt.Errorf("create user %q: %w", p.Username, err)
	}
	s.log.InfoContext(ctx, "user created", "user_id", u.ID.String(), "role", string(u.Role), "by_user_id", actor.UserID.String())
	return u, nil
}

// Update changes a user's display name, role or disabled state. Admin
// only. p.Version must be the user's current version (ErrConflict
// otherwise). Removing the last active admin (by demoting or disabling
// them) is ErrFailedPrecondition. Role changes and disabling revoke the
// user's sessions; disabling also deletes their API tokens, so enabling
// the account again does not revive them.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id domain.UserID, p UpdateParams) (User, error) {
	const op = "update user"
	if err := requireAdmin(actor, op); err != nil {
		return User{}, err
	}
	if id.IsZero() {
		return User{}, fmt.Errorf("%s: %w", op, domain.ErrInvalidID)
	}
	var v domain.Validation
	var displayName string
	if p.DisplayName != nil {
		displayName = checkDisplayName(&v, "display_name", *p.DisplayName)
	}
	if p.Role != "" && !p.Role.Valid() {
		v.Add("role", "is not a known role")
	}
	if err := v.Err(); err != nil {
		return User{}, fmt.Errorf("%s: %w", op, err)
	}

	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	cur, err := s.users.GetUser(ctx, id)
	if err != nil {
		return User{}, fmt.Errorf("%s %s: %w", op, id, err)
	}
	if cur.Version != p.Version {
		return User{}, fmt.Errorf("%s %s: at version %d, not %d: %w", op, id, cur.Version, p.Version, domain.ErrConflict)
	}
	now := s.clock.Now()
	next, changed := cur, false
	if p.DisplayName != nil && displayName != cur.DisplayName {
		next.DisplayName, changed = displayName, true
	}
	if p.Role != "" && p.Role != cur.Role {
		next.Role, changed = p.Role, true
	}
	if p.Disabled != nil && *p.Disabled != cur.Disabled() {
		next.DisabledAt, changed = time.Time{}, true
		if *p.Disabled {
			next.DisabledAt = now
		}
	}
	if !changed {
		return cur, nil
	}
	removesAdmin := activeAdmin(cur) && !activeAdmin(next)
	if removesAdmin {
		if err := s.checkNotLastAdmin(ctx, op, cur); err != nil {
			return User{}, err
		}
	}
	next.UpdatedAt = now
	stored, err := s.users.UpdateUser(ctx, next)
	if err != nil {
		return User{}, fmt.Errorf("%s %s: %w", op, id, err)
	}
	if removesAdmin {
		// adminMu covers this process; another controller removing the other
		// admin at the same time is caught here and undone.
		if err := s.undoIfNoAdminLeft(ctx, op, stored, cur); err != nil {
			return User{}, err
		}
	}
	if stored.Role != cur.Role || (stored.Disabled() && !cur.Disabled()) {
		if err := s.revokeSessions(ctx, id, nil); err != nil {
			return User{}, fmt.Errorf("%s %s: stored, but %w", op, id, err)
		}
	}
	if stored.Disabled() && !cur.Disabled() {
		if err := s.revokeAPITokens(ctx, id); err != nil {
			return User{}, fmt.Errorf("%s %s: stored, but %w", op, id, err)
		}
	}
	s.log.InfoContext(ctx, "user updated", "user_id", id.String(), "role", string(stored.Role), "disabled", stored.Disabled(),
		"by_user_id", actor.UserID.String())
	return stored, nil
}

// ResetPassword sets a new password for a user and revokes all their
// sessions. Admin only.
func (s *Service) ResetPassword(ctx context.Context, actor domain.Actor, id domain.UserID, password string) (User, error) {
	const op = "reset password"
	if err := requireAdmin(actor, op); err != nil {
		return User{}, err
	}
	if id.IsZero() {
		return User{}, fmt.Errorf("%s: %w", op, domain.ErrInvalidID)
	}
	var v domain.Validation
	checkPassword(&v, "new_password", password)
	if err := v.Err(); err != nil {
		return User{}, fmt.Errorf("%s: %w", op, err)
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return User{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.clock.Now()
	stored, err := s.modify(ctx, id, func(u User) (User, error) {
		u.PasswordHash, u.PasswordChangedAt, u.UpdatedAt = hash, now, now
		return u, nil
	})
	if err != nil {
		return User{}, fmt.Errorf("%s of user %s: %w", op, id, err)
	}
	if err := s.revokeSessions(ctx, id, nil); err != nil {
		return User{}, fmt.Errorf("%s of user %s: stored, but %w", op, id, err)
	}
	s.log.InfoContext(ctx, "user password reset", "user_id", id.String(), "by_user_id", actor.UserID.String())
	return stored, nil
}

// Delete deletes a user who owns no remotes or jobs, with their sessions
// and API tokens. Admin only. Deleting the last active admin, or a user who
// owns objects, is ErrFailedPrecondition.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.UserID) error {
	const op = "delete user"
	if err := requireAdmin(actor, op); err != nil {
		return err
	}
	if id.IsZero() {
		return fmt.Errorf("%s: %w", op, domain.ErrInvalidID)
	}
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	u, err := s.users.GetUser(ctx, id)
	if err != nil {
		return fmt.Errorf("%s %s: %w", op, id, err)
	}
	if activeAdmin(u) {
		if err := s.checkNotLastAdmin(ctx, op, u); err != nil {
			return err
		}
	}
	if err := s.users.DeleteUser(ctx, id); err != nil {
		if errors.Is(err, domain.ErrFailedPrecondition) {
			// The store's detail (constraint names) stays out of the message,
			// which reaches the caller.
			return fmt.Errorf("%s %q: %w: the user owns remotes or jobs; disable the account instead", op, u.Username, domain.ErrFailedPrecondition)
		}
		return fmt.Errorf("%s %s: %w", op, id, err)
	}
	s.log.InfoContext(ctx, "user deleted", "user_id", id.String(), "by_user_id", actor.UserID.String())
	return nil
}

// PurgeExpired deletes the sessions and API tokens that have expired, and
// any left of disabled users (disabling revokes them; this catches a
// revocation that failed). It returns how many it deleted. The composition
// root calls it periodically.
func (s *Service) PurgeExpired(ctx context.Context) (int, error) {
	now := s.clock.Now()
	n, err := s.sessions.DeleteExpiredSessions(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("purge expired sessions: %w", err)
	}
	m, err := s.tokens.DeleteExpiredAPITokens(ctx, now)
	if err != nil {
		return n, fmt.Errorf("purge expired api tokens: %w", err)
	}
	n += m
	us, err := s.users.ListUsers(ctx)
	if err != nil {
		return n, fmt.Errorf("purge credentials of disabled users: %w", err)
	}
	for _, u := range us {
		if !u.Disabled() {
			continue
		}
		m, err := s.sessions.DeleteUserSessions(ctx, u.ID, nil)
		if err != nil {
			return n, fmt.Errorf("purge sessions of disabled user %s: %w", u.ID, err)
		}
		n += m
		if m, err = s.tokens.DeleteUserAPITokens(ctx, u.ID); err != nil {
			return n, fmt.Errorf("purge api tokens of disabled user %s: %w", u.ID, err)
		}
		n += m
	}
	return n, nil
}

// Active reports whether the user exists and is not disabled; the
// scheduler asks before it launches a schedule's job for its owner.
// ErrNotFound if the user does not exist.
func (s *Service) Active(ctx context.Context, id domain.UserID) (bool, error) {
	if id.IsZero() {
		return false, fmt.Errorf("user active: %w", domain.ErrInvalidID)
	}
	u, err := s.users.GetUser(ctx, id)
	if err != nil {
		return false, fmt.Errorf("user active %s: %w", id, err)
	}
	return !u.Disabled(), nil
}

// requireAdmin is the use cases' own role check (the API's policy table
// checks it first; this is defense in depth).
func requireAdmin(actor domain.Actor, op string) error {
	if !actor.IsAdmin() {
		return fmt.Errorf("%s: %w: admins only", op, domain.ErrPermissionDenied)
	}
	return nil
}

func activeAdmin(u User) bool { return u.Role == domain.RoleAdmin && !u.Disabled() }

// checkNotLastAdmin refuses a change that removes u, an active admin, from
// the active admins if no other is left. Callers hold adminMu.
func (s *Service) checkNotLastAdmin(ctx context.Context, op string, u User) error {
	n, err := s.users.CountActiveAdmins(ctx)
	if err != nil {
		return fmt.Errorf("%s %s: %w", op, u.ID, err)
	}
	if n <= 1 {
		return lastAdminError(op, u)
	}
	return nil
}

func lastAdminError(op string, u User) error {
	return fmt.Errorf("%s %q: %w: they are the last active admin; make another user an admin first", op, u.Username, domain.ErrFailedPrecondition)
}

// undoIfNoAdminLeft restores before's role and disabled state if stored
// left no active admin (a concurrent change elsewhere removed the others),
// and then reports the last-admin error.
func (s *Service) undoIfNoAdminLeft(ctx context.Context, op string, stored, before User) error {
	// A failed recount does not fail the stored change.
	if n, err := s.users.CountActiveAdmins(ctx); err != nil || n > 0 {
		return nil
	}
	undo := stored
	undo.Role, undo.DisabledAt, undo.UpdatedAt = before.Role, before.DisabledAt, s.clock.Now()
	if _, err := s.users.UpdateUser(ctx, undo); err != nil {
		s.log.ErrorContext(ctx, "cannot undo a change that left no active admin", "user_id", stored.ID.String(), "error", err.Error())
	}
	return lastAdminError(op, before)
}

// revokeSessions deletes the user's sessions except keep.
func (s *Service) revokeSessions(ctx context.Context, user domain.UserID, keep *TokenHash) error {
	n, err := s.sessions.DeleteUserSessions(ctx, user, keep)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if n > 0 {
		s.log.InfoContext(ctx, "user sessions revoked", "user_id", user.String(), "count", n)
	}
	return nil
}

// revokeAPITokens deletes every API token of the user.
func (s *Service) revokeAPITokens(ctx context.Context, user domain.UserID) error {
	n, err := s.tokens.DeleteUserAPITokens(ctx, user)
	if err != nil {
		return fmt.Errorf("revoke api tokens: %w", err)
	}
	if n > 0 {
		s.log.InfoContext(ctx, "user api tokens revoked", "user_id", user.String(), "count", n)
	}
	return nil
}

// maxUpdateAttempts bounds the re-read and retry of internal updates that
// lose a race (ErrConflict).
const maxUpdateAttempts = 5

// modify applies change to the stored user with a version-checked update,
// re-reading and re-applying it when another change won the race. change
// may refuse (its error is returned as is).
func (s *Service) modify(ctx context.Context, id domain.UserID, change func(User) (User, error)) (User, error) {
	for attempt := 1; ; attempt++ {
		cur, err := s.users.GetUser(ctx, id)
		if err != nil {
			return User{}, err
		}
		next, err := change(cur)
		if err != nil {
			return User{}, err
		}
		next.Version = cur.Version
		stored, err := s.users.UpdateUser(ctx, next)
		if err == nil {
			return stored, nil
		}
		if !errors.Is(err, domain.ErrConflict) || attempt == maxUpdateAttempts {
			return User{}, err
		}
	}
}

// randomToken returns 32 random bytes as base64url without padding (43
// characters).
func (s *Service) randomToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := io.ReadFull(s.random, b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenBytes is the entropy of session and API tokens; tokenLen is its
// encoded length.
const (
	tokenBytes = 32
	tokenLen   = 43
)

// isToken reports whether s has the shape of an encoded random token.
func isToken(s string) bool {
	if len(s) != tokenLen {
		return false
	}
	for i := range len(s) {
		if !isBase64URL(s[i]) {
			return false
		}
	}
	return true
}

// isBase64URL reports whether c is in the base64url alphabet.
func isBase64URL(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

// Input validation. Messages reach the caller next to the field.

// validUsername reports whether name is a canonical username:
// ^[a-z0-9][a-z0-9._-]{0,63}$.
func validUsername(name string) bool {
	if name == "" || len(name) > MaxUsernameLen {
		return false
	}
	for i := range len(name) {
		c := name[i]
		alnum := 'a' <= c && c <= 'z' || '0' <= c && c <= '9'
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != '-')) {
			return false
		}
	}
	return true
}

func checkUsername(v *domain.Validation, field, name string) {
	switch {
	case name == "":
		v.Add(field, "is required")
	case len(name) > MaxUsernameLen:
		v.Add(field, fmt.Sprintf("must be at most %d characters", MaxUsernameLen))
	case !validUsername(name):
		v.Add(field, "must be lower case letters, digits, \".\", \"_\" or \"-\", starting with a letter or digit")
	}
}

// checkDisplayName returns the trimmed display name.
func checkDisplayName(v *domain.Validation, field, name string) string {
	name = strings.TrimSpace(name)
	switch {
	case utf8.RuneCountInString(name) > MaxDisplayNameLen:
		v.Add(field, fmt.Sprintf("must be at most %d characters", MaxDisplayNameLen))
	case hasControl(name):
		v.Add(field, "must not contain control characters")
	}
	return name
}

func checkPassword(v *domain.Validation, field, password string) {
	switch n := utf8.RuneCountInString(password); {
	case n < MinPasswordLen:
		v.Add(field, fmt.Sprintf("must be at least %d characters", MinPasswordLen))
	case n > MaxPasswordLen:
		v.Add(field, fmt.Sprintf("must be at most %d characters", MaxPasswordLen))
	}
}

func hasControl(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl)
}

// cleanText makes request metadata (user agent, client address) safe to
// store and display: valid UTF-8, no control characters, at most n
// characters.
func cleanText(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s
}
