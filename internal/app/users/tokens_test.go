package users_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestCreateAndUseAPIToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleAdmin)

	tok, secret, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), "  backup script ", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^syncr_pat_[A-Za-z0-9_-]{43}$`).MatchString(secret) {
		t.Fatalf("secret %q has the wrong shape", secret)
	}
	if tok.Prefix != secret[:14] || len(tok.Prefix) != users.APITokenDisplayLen || tok.Name != "backup script" ||
		tok.UserID != alice.ID || !tok.CreatedAt.Equal(t0()) || !tok.ExpiresAt.IsZero() || tok.Hash != users.HashToken(secret) {
		t.Errorf("token = %+v", tok)
	}
	// The store has the hash, never the secret.
	stored, err := e.store.GetAPITokenByHash(e.ctx(), users.HashToken(secret))
	if err != nil || stored.ID != tok.ID {
		t.Fatalf("stored token: (%+v, %v)", stored, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", stored), secret[len(users.APITokenPrefix):]) {
		t.Error("the stored token contains the secret")
	}
	if strings.Contains(e.logs.String(), secret[len(users.APITokenPrefix):]) {
		t.Error("the secret was logged")
	}

	actor, got, err := e.svc.AuthenticateAPIToken(e.ctx(), secret)
	if err != nil || actor != alice.Actor() || got.ID != tok.ID || !got.LastUsedAt.Equal(t0()) {
		t.Fatalf("AuthenticateAPIToken = (%+v, %+v, %v)", actor, got, err)
	}
	// The last use is stored at most once a minute.
	e.clock.Advance(30 * time.Second)
	if _, _, err := e.svc.AuthenticateAPIToken(e.ctx(), secret); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.store.GetAPITokenByHash(e.ctx(), tok.Hash); !s.LastUsedAt.Equal(t0()) {
		t.Errorf("touched within a minute: %v", s.LastUsedAt)
	}
	at := e.clock.Advance(30 * time.Second)
	if _, _, err := e.svc.AuthenticateAPIToken(e.ctx(), secret); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.store.GetAPITokenByHash(e.ctx(), tok.Hash); !s.LastUsedAt.Equal(at) {
		t.Errorf("not touched after a minute: %v", s.LastUsedAt)
	}

	// A token acts with its owner's current role, and stops with the
	// account.
	root := e.addUser("root", domain.RoleAdmin)
	u, err := e.svc.Update(e.ctx(), root.Actor(), alice.ID, users.UpdateParams{Version: e.get(alice.ID).Version, Role: domain.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	if actor, _, _ := e.svc.AuthenticateAPIToken(e.ctx(), secret); actor.Role != domain.RoleUser {
		t.Errorf("demoted owner's token acts as %q", actor.Role)
	}
	if u, err = e.svc.Update(e.ctx(), root.Actor(), alice.ID, users.UpdateParams{Version: u.Version, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.AuthenticateAPIToken(e.ctx(), secret)
	wantErr(t, "AuthenticateAPIToken(disabled owner)", err, domain.ErrUnauthenticated)
	if _, err = e.svc.Update(e.ctx(), root.Actor(), alice.ID, users.UpdateParams{Version: u.Version, Disabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.AuthenticateAPIToken(e.ctx(), secret); err != nil {
		t.Errorf("re-enabled owner's token: %v", err)
	}
}

func TestAPITokenExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	tok, secret, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), "short lived", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.ExpiresAt.Equal(t0().Add(time.Hour)) {
		t.Errorf("ExpiresAt = %v", tok.ExpiresAt)
	}
	e.clock.Advance(time.Hour - time.Second)
	if _, got, err := e.svc.AuthenticateAPIToken(e.ctx(), secret); err != nil || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("before expiry: (%+v, %v)", got, err)
	}
	e.clock.Advance(time.Second)
	_, _, err = e.svc.AuthenticateAPIToken(e.ctx(), secret)
	wantErr(t, "AuthenticateAPIToken(expired)", err, domain.ErrUnauthenticated)
}

func TestAuthenticateAPITokenRejects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	_, secret, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	session := e.login("alice")
	for name, bad := range map[string]string{
		"empty":         "",
		"no prefix":     secret[len(users.APITokenPrefix):],
		"session token": session,
		"other prefix":  "ghp_" + secret[len(users.APITokenPrefix):],
		"truncated":     secret[:len(secret)-1],
		"extended":      secret + "A",
		"bad character": secret[:len(secret)-1] + "=",
		"unknown":       users.APITokenPrefix + strings.Repeat("A", 43),
	} {
		_, _, err := e.svc.AuthenticateAPIToken(e.ctx(), bad)
		wantErr(t, name, err, domain.ErrUnauthenticated)
	}
}

func TestAPITokenValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(_ *users.Deps, c *users.Config) { c.MaxAPITokens = 2 })
	alice := e.addUser("alice", domain.RoleUser)
	tests := []struct {
		name, tokenName string
		ttl             time.Duration
		field           string
	}{
		{"empty name", "  ", 0, "name"},
		{"long name", strings.Repeat("n", 65), 0, "name"},
		{"control in name", "ci\tbot", 0, "name"},
		{"negative ttl", "ci", -time.Hour, "ttl"},
		{"tiny ttl", "ci", time.Second, "ttl"},
		{"huge ttl", "ci", 11 * 365 * day, "ttl"},
	}
	for _, tt := range tests {
		_, _, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), tt.tokenName, tt.ttl)
		wantFields(t, tt.name, err, tt.field)
	}
	_, _, err := e.svc.CreateAPIToken(e.ctx(), domain.Actor{Role: domain.RoleAdmin}, "ci", 0)
	wantErr(t, "CreateAPIToken(no account)", err, domain.ErrPermissionDenied)

	for _, name := range []string{strings.Repeat("n", 64), "second"} {
		if _, _, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), name, time.Minute); err != nil {
			t.Fatalf("CreateAPIToken(%q): %v", name, err)
		}
	}
	_, _, err = e.svc.CreateAPIToken(e.ctx(), alice.Actor(), "third", 0)
	wantErr(t, "CreateAPIToken(over the maximum)", err, domain.ErrFailedPrecondition)
}

func TestListAndDeleteAPITokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	root := e.addUser("root", domain.RoleAdmin)
	first, _, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), "first", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, secret, err := e.svc.CreateAPIToken(e.ctx(), alice.Actor(), "second", 0)
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListAPITokens(e.ctx(), alice.Actor())
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("ListAPITokens = (%v, %v), want newest first", list, err)
	}
	if list, err := e.svc.ListAPITokens(e.ctx(), root.Actor()); err != nil || len(list) != 0 {
		t.Errorf("another user's list = (%v, %v)", list, err)
	}
	if list, err := e.svc.ListAPITokens(e.ctx(), domain.Actor{}); err != nil || list == nil || len(list) != 0 {
		t.Errorf("ListAPITokens(nobody) = (%v, %v)", list, err)
	}

	// Tokens are private, admins included.
	wantErr(t, "DeleteAPIToken(an admin, another user's)", e.svc.DeleteAPIToken(e.ctx(), root.Actor(), second.ID), domain.ErrNotFound)
	wantErr(t, "DeleteAPIToken(zero)", e.svc.DeleteAPIToken(e.ctx(), alice.Actor(), domain.APITokenID{}), domain.ErrInvalidID)
	if err := e.svc.DeleteAPIToken(e.ctx(), alice.Actor(), second.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.AuthenticateAPIToken(e.ctx(), secret)
	wantErr(t, "AuthenticateAPIToken(deleted)", err, domain.ErrUnauthenticated)
	wantErr(t, "DeleteAPIToken(again)", e.svc.DeleteAPIToken(e.ctx(), alice.Actor(), second.ID), domain.ErrNotFound)
}
