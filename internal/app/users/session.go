package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Lengths of the request metadata stored with a session.
const (
	maxClientIPLen  = 64
	maxUserAgentLen = 256
)

// invalidUsernameKey is the username limiter's key for every login with a
// username that cannot exist: their number is unbounded, so they share one
// bucket instead of growing the limiter.
const invalidUsernameKey = "\x00invalid"

// errCredentialsChanged aborts a login or password change whose user
// changed password or was disabled while it ran.
var errCredentialsChanged = errors.New("the account's credentials changed meanwhile")

// Login checks a username and password and starts a session.
//
// Attempts are rate limited per client IP and per username, unknown
// usernames included (ErrRateLimited; every attempt counts). The username
// is matched case-insensitively. Wrong credentials, an unknown username
// and a disabled account are all ErrUnauthenticated, and all cost one
// password verification (unknown usernames are verified against a dummy
// hash), so neither the error nor the timing tells them apart. A password
// hash with outdated parameters is replaced.
func (s *Service) Login(ctx context.Context, p LoginParams) (User, IssuedSession, error) {
	const op = "login"
	ip := cleanText(p.ClientIP, maxClientIPLen)
	if !s.limits.ClientIP.Allow(ip) {
		s.log.WarnContext(ctx, "login rate limited", "client_ip", ip, "limit", "client_ip")
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, domain.ErrRateLimited)
	}
	username := strings.ToLower(strings.TrimSpace(p.Username))
	known := validUsername(username)
	limitKey := username
	if !known {
		limitKey = invalidUsernameKey
	}
	if !s.limits.Username.Allow(limitKey) {
		s.log.WarnContext(ctx, "login rate limited", "client_ip", ip, "limit", "username")
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, domain.ErrRateLimited)
	}

	var u User
	if known {
		var err error
		u, err = s.users.GetUserByUsername(ctx, username)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			known = false
		case err != nil:
			return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	encoded := s.dummyHash
	if known {
		encoded = u.PasswordHash
	}
	ok, rehash, err := s.verify(ctx, p.Password, encoded)
	if err != nil {
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	var reason string
	switch {
	case !known:
		reason = "unknown_user"
	case !ok:
		reason = "wrong_password"
	case u.Disabled():
		reason = "disabled"
	}
	if reason != "" {
		// Unknown usernames are not logged: they are often mistyped
		// passwords.
		if known {
			s.log.InfoContext(ctx, "login failed", "client_ip", ip, "reason", reason, "user_id", u.ID.String())
		} else {
			s.log.InfoContext(ctx, "login failed", "client_ip", ip, "reason", reason)
		}
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w: %s", op, domain.ErrUnauthenticated, reason)
	}

	var newHash string
	if rehash {
		if newHash, err = s.hasher.Hash(ctx, p.Password); err != nil {
			s.log.WarnContext(ctx, "cannot rehash a password with outdated parameters", "user_id", u.ID.String(), "error", err.Error())
			newHash = ""
		}
	}
	// The session is created first and the login recorded after, with a
	// check that the credentials are the ones verified: a password change,
	// reset or disabling that races with this login either revokes the new
	// session or makes the check fail.
	issued, hash, err := s.startSession(ctx, u.ID, ip, p.UserAgent)
	if err != nil {
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.clock.Now()
	stored, err := s.modify(ctx, u.ID, func(cur User) (User, error) {
		if cur.PasswordHash != u.PasswordHash || cur.Disabled() {
			return User{}, errCredentialsChanged
		}
		cur.LastLoginAt = now
		if newHash != "" {
			cur.PasswordHash = newHash
		}
		return cur, nil
	})
	if err != nil {
		if derr := s.sessions.DeleteSession(ctx, hash); derr != nil {
			s.log.ErrorContext(ctx, "cannot delete the session of a failed login", "user_id", u.ID.String(), "error", derr.Error())
		}
		if errors.Is(err, errCredentialsChanged) || errors.Is(err, domain.ErrNotFound) {
			return User{}, IssuedSession{}, fmt.Errorf("%s: %w: %w", op, domain.ErrUnauthenticated, err)
		}
		return User{}, IssuedSession{}, fmt.Errorf("%s: record the login: %w", op, err)
	}
	if newHash != "" {
		s.log.InfoContext(ctx, "password rehashed with the current parameters", "user_id", u.ID.String())
	}
	s.log.InfoContext(ctx, "login", "user_id", u.ID.String(), "client_ip", ip)
	return stored, issued, nil
}

// verify checks password against encoded. A password longer than any
// accepted one cannot match; it is replaced by an empty one, so the
// verification still costs the same (and the hash input stays small).
func (s *Service) verify(ctx context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	tooLong := len(password) > 4*MaxPasswordLen || utf8.RuneCountInString(password) > MaxPasswordLen
	if tooLong {
		password = ""
	}
	ok, needsRehash, err = s.hasher.Verify(ctx, password, encoded)
	if err != nil {
		return false, false, fmt.Errorf("verify password: %w", err)
	}
	return ok && !tooLong, needsRehash, nil
}

// startSession creates a session for user and returns it as issued to the
// browser, with its hash.
func (s *Service) startSession(ctx context.Context, user domain.UserID, ip, userAgent string) (IssuedSession, TokenHash, error) {
	token, err := s.randomToken()
	if err != nil {
		return IssuedSession{}, TokenHash{}, fmt.Errorf("start session: %w", err)
	}
	now := s.clock.Now()
	sess := Session{
		IDHash: HashToken(token), UserID: user, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(s.cfg.SessionMaxAge),
		UserAgent: cleanText(userAgent, maxUserAgentLen), ClientIP: cleanText(ip, maxClientIPLen),
	}
	sess.IdleExpiresAt = earliest(now.Add(s.cfg.SessionIdleTimeout), sess.ExpiresAt)
	if err := s.sessions.CreateSession(ctx, sess); err != nil {
		return IssuedSession{}, TokenHash{}, fmt.Errorf("start session: %w", err)
	}
	return IssuedSession{Token: token, ExpiresAt: sess.ExpiresAt, MaxAge: s.cfg.SessionMaxAge}, sess.IDHash, nil
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// Logout ends the session of a raw session token. Ending an unknown or
// malformed session is not an error.
func (s *Service) Logout(ctx context.Context, sessionToken string) error {
	if !isToken(sessionToken) {
		return nil
	}
	if err := s.sessions.DeleteSession(ctx, HashToken(sessionToken)); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// AuthenticateSession resolves a raw session token (the cookie value) to
// the actor of its user, with the user's current role.
//
// It is ErrUnauthenticated for a malformed or unknown token, a session
// that is idle for longer than the idle timeout or older than the maximum
// age, and a disabled user; expired sessions and those of disabled users
// are deleted on the way. Use extends the idle expiry (never beyond the
// absolute one), stored at most once per TouchInterval. The returned
// session reflects that.
func (s *Service) AuthenticateSession(ctx context.Context, token string) (domain.Actor, Session, error) {
	const op = "authenticate session"
	if !isToken(token) {
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w: malformed token", op, domain.ErrUnauthenticated)
	}
	h := HashToken(token)
	sess, err := s.sessions.GetSession(ctx, h)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w: unknown session", op, domain.ErrUnauthenticated)
	}
	if err != nil {
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.clock.Now()
	if sess.Expired(now) {
		s.dropSession(ctx, h)
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w: session expired", op, domain.ErrUnauthenticated)
	}
	u, err := s.users.GetUser(ctx, sess.UserID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		s.dropSession(ctx, h)
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w: the user no longer exists", op, domain.ErrUnauthenticated)
	case err != nil:
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w", op, err)
	case u.Disabled():
		s.dropSession(ctx, h)
		return domain.Actor{}, Session{}, fmt.Errorf("%s: %w: account disabled", op, domain.ErrUnauthenticated)
	}
	if now.Sub(sess.LastSeenAt) >= s.cfg.TouchInterval {
		idle := earliest(now.Add(s.cfg.SessionIdleTimeout), sess.ExpiresAt)
		err := s.sessions.TouchSession(ctx, h, now, idle)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Actor{}, Session{}, fmt.Errorf("%s: %w: session ended", op, domain.ErrUnauthenticated)
		}
		if err != nil {
			return domain.Actor{}, Session{}, fmt.Errorf("%s: %w", op, err)
		}
		sess.LastSeenAt, sess.IdleExpiresAt = now, idle
	}
	return u.Actor(), sess, nil
}

// dropSession deletes a session that can no longer be used; a failure is
// only logged (PurgeExpired catches it later).
func (s *Service) dropSession(ctx context.Context, h TokenHash) {
	if err := s.sessions.DeleteSession(ctx, h); err != nil {
		s.log.WarnContext(ctx, "cannot delete an unusable session", "error", err.Error())
	}
}

// ChangePassword checks the actor's current password, stores the new one
// and revokes the actor's other sessions: every one but sessionToken's (an
// empty sessionToken keeps none).
//
// A wrong current password is a *domain.ValidationError on
// "current_password" (not ErrUnauthenticated: the caller is
// authenticated); attempts share the username rate limit of Login.
func (s *Service) ChangePassword(ctx context.Context, actor domain.Actor, sessionToken, current, next string) error {
	const op = "change password"
	if actor.UserID.IsZero() {
		return fmt.Errorf("%s: %w: the caller has no account", op, domain.ErrPermissionDenied)
	}
	var v domain.Validation
	checkPassword(&v, "new_password", next)
	if err := v.Err(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	u, err := s.users.GetUser(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !s.limits.Username.Allow(u.Username) {
		s.log.WarnContext(ctx, "password change rate limited", "user_id", u.ID.String())
		return fmt.Errorf("%s: %w", op, domain.ErrRateLimited)
	}
	ok, _, err := s.verify(ctx, current, u.PasswordHash)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !ok {
		s.log.InfoContext(ctx, "password change refused: wrong current password", "user_id", u.ID.String())
		v.Add("current_password", "is wrong")
		return fmt.Errorf("%s: %w", op, v.Err())
	}
	hash, err := s.hasher.Hash(ctx, next)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	now := s.clock.Now()
	_, err = s.modify(ctx, u.ID, func(cur User) (User, error) {
		if cur.PasswordHash != u.PasswordHash {
			return User{}, errCredentialsChanged
		}
		cur.PasswordHash, cur.PasswordChangedAt, cur.UpdatedAt = hash, now, now
		return cur, nil
	})
	if errors.Is(err, errCredentialsChanged) {
		return fmt.Errorf("%s: %w: %w", op, domain.ErrConflict, err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	var keep *TokenHash
	if sessionToken != "" {
		h := HashToken(sessionToken)
		keep = &h
	}
	if err := s.revokeSessions(ctx, u.ID, keep); err != nil {
		return fmt.Errorf("%s: stored, but %w", op, err)
	}
	s.log.InfoContext(ctx, "password changed", "user_id", u.ID.String())
	return nil
}
