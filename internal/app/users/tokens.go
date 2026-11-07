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

// API token format.
const (
	// APITokenPrefix starts every personal API token, so that scanners and
	// people recognize one.
	APITokenPrefix = "syncr_pat_"
	// APITokenDisplayLen is the length of APIToken.Prefix: APITokenPrefix
	// and the first four random characters.
	APITokenDisplayLen = len(APITokenPrefix) + 4
)

// CreateAPIToken mints a personal API token for the actor. The raw secret
// is returned only here; the store keeps its SHA-256 hash. name is trimmed
// and must have 1 to MaxTokenNameLen characters; ttl 0 means the token
// never expires, otherwise it must be between MinAPITokenTTL and
// MaxAPITokenTTL. A user may hold at most Config.MaxAPITokens tokens
// (ErrFailedPrecondition beyond).
//
// The API allows it only to callers with a session, so that a leaked token
// cannot mint more.
func (s *Service) CreateAPIToken(ctx context.Context, actor domain.Actor, name string, ttl time.Duration) (APIToken, string, error) {
	const op = "create api token"
	if actor.UserID.IsZero() {
		return APIToken{}, "", fmt.Errorf("%s: %w: the caller has no account", op, domain.ErrPermissionDenied)
	}
	var v domain.Validation
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		v.Add("name", "is required")
	case utf8.RuneCountInString(name) > MaxTokenNameLen:
		v.Add("name", fmt.Sprintf("must be at most %d characters", MaxTokenNameLen))
	case hasControl(name):
		v.Add("name", "must not contain control characters")
	}
	switch {
	case ttl < 0:
		v.Add("ttl", "must not be negative")
	case ttl > 0 && ttl < MinAPITokenTTL:
		v.Add("ttl", "must be at least a minute (or unset for no expiry)")
	case ttl > MaxAPITokenTTL:
		v.Add("ttl", "must be at most ten years (or unset for no expiry)")
	}
	if err := v.Err(); err != nil {
		return APIToken{}, "", fmt.Errorf("%s: %w", op, err)
	}
	existing, err := s.tokens.ListAPITokens(ctx, actor.UserID)
	if err != nil {
		return APIToken{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if len(existing) >= s.cfg.MaxAPITokens {
		return APIToken{}, "", fmt.Errorf("%s: %w: you have %d API tokens, the most allowed; delete unused ones first",
			op, domain.ErrFailedPrecondition, len(existing))
	}
	random, err := s.randomToken()
	if err != nil {
		return APIToken{}, "", fmt.Errorf("%s: %w", op, err)
	}
	secret := APITokenPrefix + random
	now := s.clock.Now()
	t := APIToken{
		ID: s.ids.NewAPITokenID(), UserID: actor.UserID, Name: name, Prefix: secret[:APITokenDisplayLen],
		Hash: HashToken(secret), CreatedAt: now,
	}
	if ttl > 0 {
		t.ExpiresAt = now.Add(ttl)
	}
	if err := s.tokens.CreateAPIToken(ctx, t); err != nil {
		return APIToken{}, "", fmt.Errorf("%s: %w", op, err)
	}
	s.log.InfoContext(ctx, "api token created", "user_id", actor.UserID.String(), "token_id", t.ID.String(), "token_prefix", t.Prefix)
	return t, secret, nil
}

// ListAPITokens returns the actor's API tokens, newest first.
func (s *Service) ListAPITokens(ctx context.Context, actor domain.Actor) ([]APIToken, error) {
	if actor.UserID.IsZero() {
		return []APIToken{}, nil
	}
	ts, err := s.tokens.ListAPITokens(ctx, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	return ts, nil
}

// DeleteAPIToken revokes one of the actor's API tokens. Another user's
// token is ErrNotFound, admins included.
func (s *Service) DeleteAPIToken(ctx context.Context, actor domain.Actor, id domain.APITokenID) error {
	const op = "delete api token"
	if id.IsZero() {
		return fmt.Errorf("%s: %w", op, domain.ErrInvalidID)
	}
	if err := s.tokens.DeleteAPIToken(ctx, actor.UserID, id); err != nil {
		return fmt.Errorf("%s %s: %w", op, id, err)
	}
	s.log.InfoContext(ctx, "api token deleted", "user_id", actor.UserID.String(), "token_id", id.String())
	return nil
}

// AuthenticateAPIToken resolves a raw personal API token (a bearer token)
// to the actor of its owner, with the owner's current role; the token is
// returned for its expiry.
//
// It is ErrUnauthenticated for a malformed, unknown or expired token and
// for a disabled owner. The token's last use is stored at most once per
// TouchInterval. The lookup is by SHA-256 hash: tokens carry 256 random
// bits, so timing differences of the lookup reveal nothing usable.
func (s *Service) AuthenticateAPIToken(ctx context.Context, token string) (domain.Actor, APIToken, error) {
	const op = "authenticate api token"
	random, ok := strings.CutPrefix(token, APITokenPrefix)
	if !ok || !isToken(random) {
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w: malformed token", op, domain.ErrUnauthenticated)
	}
	t, err := s.tokens.GetAPITokenByHash(ctx, HashToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w: unknown token", op, domain.ErrUnauthenticated)
	}
	if err != nil {
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.clock.Now()
	if t.Expired(now) {
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w: token %s expired", op, domain.ErrUnauthenticated, t.ID)
	}
	u, err := s.users.GetUser(ctx, t.UserID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w: the owner no longer exists", op, domain.ErrUnauthenticated)
	case err != nil:
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w", op, err)
	case u.Disabled():
		return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w: token %s: account disabled", op, domain.ErrUnauthenticated, t.ID)
	}
	if t.LastUsedAt.IsZero() || now.Sub(t.LastUsedAt) >= s.cfg.TouchInterval {
		err := s.tokens.TouchAPIToken(ctx, t.ID, now)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w: token %s was deleted", op, domain.ErrUnauthenticated, t.ID)
		}
		if err != nil {
			return domain.Actor{}, APIToken{}, fmt.Errorf("%s: %w", op, err)
		}
		t.LastUsedAt = now
	}
	return u.Actor(), t, nil
}
