package users

import (
	"context"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Setup codes.
const (
	// MinSetupCodeLen is the shortest setup code ArmSetup accepts, in
	// characters other than dashes and spaces (a preset auth.setup_code).
	MinSetupCodeLen = 16

	// setupCodeChars is the length of a generated code: 20 base32
	// characters carry 100 bits.
	setupCodeChars = 20
	// setupGroup is the length of the dash-separated groups it is shown in.
	setupGroup = 5
	// setupLimitKey is the key of the global setup limiter.
	setupLimitKey = "setup"
)

// NewSetupCode returns a random setup code of 100 bits, as 20 base32
// characters (A-Z, 2-7) in groups of five: XXXXX-XXXXX-XXXXX-XXXXX. The
// composition root arms it (ArmSetup) and logs it at start while no user
// exists.
func NewSetupCode(random io.Reader) (string, error) {
	b := make([]byte, 13) // 104 bits; the first 100 are used
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("new setup code: read random bytes: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:setupCodeChars]
	groups := make([]string, 0, setupCodeChars/setupGroup)
	for i := 0; i < setupCodeChars; i += setupGroup {
		groups = append(groups, enc[i:i+setupGroup])
	}
	return strings.Join(groups, "-"), nil
}

// normalizeSetupCode drops dashes and white space and upper-cases code, so
// that "abcde-fghij" and "ABCDEFGHIJ" are the same code.
func normalizeSetupCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, code)
}

// ArmSetup sets the one-time setup code CompleteSetup asks for, replacing
// any armed before. Only its hash is kept. The code must have at least
// MinSetupCodeLen characters besides dashes and spaces; errors never
// contain it. The composition root calls it at start while SetupRequired
// (NewSetupCode, or the preset auth.setup_code).
func (s *Service) ArmSetup(code string) error {
	norm := normalizeSetupCode(code)
	if n := utf8.RuneCountInString(norm); n < MinSetupCodeLen {
		return fmt.Errorf("arm setup: the setup code has %d characters, want at least %d", n, MinSetupCodeLen)
	}
	h := HashToken(norm)
	s.setupMu.Lock()
	s.setupCode = &h
	s.setupMu.Unlock()
	return nil
}

// armedSetupCode returns the armed code's hash, or nil.
func (s *Service) armedSetupCode() *TokenHash {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupCode
}

// SetupRequired reports whether no user exists yet. Once a user exists the
// answer is cached: the count never drops back to zero.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	if s.setupDone.Load() {
		return false, nil
	}
	n, err := s.users.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("setup status: %w", err)
	}
	if n > 0 {
		s.setupDone.Store(true)
		return false, nil
	}
	return true, nil
}

// setupCompleteError is the error of setup attempts once a user exists.
func setupCompleteError(op string) error {
	return fmt.Errorf("%s: %w: setup is already complete; log in instead", op, domain.ErrFailedPrecondition)
}

// CompleteSetup creates the first admin and starts their session.
//
// It is ErrFailedPrecondition once a user exists or while no setup code is
// armed, and ErrRateLimited beyond the per-client-IP and global setup
// limits (every attempt counts). A wrong code is a *domain.ValidationError
// on "setup_code", alongside any other invalid field; the code is compared
// in constant time (as SHA-256 digests, so its length does not leak
// either). Of concurrent calls with the right code exactly one creates the
// admin (Repository.CreateFirstUser); the others get ErrFailedPrecondition.
func (s *Service) CompleteSetup(ctx context.Context, p SetupParams) (User, IssuedSession, error) {
	const op = "complete setup"
	required, err := s.SetupRequired(ctx)
	if err != nil {
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	if !required {
		return User{}, IssuedSession{}, setupCompleteError(op)
	}
	armed := s.armedSetupCode()
	if armed == nil {
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w: no setup code is armed; restart the controller and use the code it logs",
			op, domain.ErrFailedPrecondition)
	}
	ip := cleanText(p.ClientIP, maxClientIPLen)
	if !s.limits.ClientIP.Allow(ip) || !s.limits.Setup.Allow(setupLimitKey) {
		s.log.WarnContext(ctx, "setup attempt rate limited", "client_ip", ip)
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, domain.ErrRateLimited)
	}

	var v domain.Validation
	got := HashToken(normalizeSetupCode(p.Code))
	codeOK := subtle.ConstantTimeCompare(got[:], armed[:]) == 1
	if !codeOK {
		v.Add("setup_code", "is wrong")
	}
	checkUsername(&v, "username", p.Username)
	displayName := checkDisplayName(&v, "display_name", p.DisplayName)
	checkPassword(&v, "password", p.Password)
	if err := v.Err(); err != nil {
		if !codeOK {
			s.log.WarnContext(ctx, "setup attempt with a wrong setup code", "client_ip", ip)
		}
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
	}

	hash, err := s.hasher.Hash(ctx, p.Password)
	if err != nil {
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.clock.Now()
	u := User{
		ID: s.ids.NewUserID(), Username: p.Username, DisplayName: displayName, Role: domain.RoleAdmin, PasswordHash: hash,
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, LastLoginAt: now, Version: 1,
	}
	if err := s.users.CreateFirstUser(ctx, u); err != nil {
		if errors.Is(err, domain.ErrFailedPrecondition) {
			s.setupDone.Store(true)
			return User{}, IssuedSession{}, setupCompleteError(op)
		}
		return User{}, IssuedSession{}, fmt.Errorf("%s: %w", op, err)
	}
	s.setupDone.Store(true)
	s.setupMu.Lock()
	s.setupCode = nil
	s.setupMu.Unlock()
	s.log.WarnContext(ctx, "first-run setup complete: admin created", "user_id", u.ID.String(), "client_ip", ip)

	issued, _, err := s.startSession(ctx, u.ID, ip, p.UserAgent)
	if err != nil {
		return User{}, IssuedSession{}, fmt.Errorf("%s: the admin was created, but %w", op, err)
	}
	return u, issued, nil
}
