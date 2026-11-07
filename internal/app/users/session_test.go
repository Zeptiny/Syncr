package users_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleAdmin)
	e.clock.Advance(time.Hour)

	u, s, err := e.svc.Login(e.ctx(), users.LoginParams{
		Username: "  Alice ", Password: goodPass, ClientIP: testIP, UserAgent: "agent\x00with\ncontrols/" + strings.Repeat("x", 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := t0().Add(time.Hour)
	if u.ID != alice.ID || !u.LastLoginAt.Equal(now) || u.Version != 2 {
		t.Errorf("Login returned %+v", u)
	}
	if len(s.Token) != 43 || !s.ExpiresAt.Equal(now.Add(30*day)) || s.MaxAge != 30*day {
		t.Errorf("issued %+v", s)
	}
	sess, err := e.store.GetSession(e.ctx(), users.HashToken(s.Token))
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID != alice.ID || !sess.CreatedAt.Equal(now) || !sess.LastSeenAt.Equal(now) ||
		!sess.IdleExpiresAt.Equal(now.Add(7*day)) || !sess.ExpiresAt.Equal(now.Add(30*day)) || sess.ClientIP != testIP {
		t.Errorf("stored session %+v", sess)
	}
	if strings.ContainsAny(sess.UserAgent, "\x00\n") || len([]rune(sess.UserAgent)) != 256 || !strings.HasPrefix(sess.UserAgent, "agentwithcontrols/") {
		t.Errorf("user agent stored as %q", sess.UserAgent)
	}
	// Only the hash of the token is stored.
	if sess.IDHash != users.HashToken(s.Token) {
		t.Error("the session is not keyed by the token's hash")
	}
}

// TestLoginFailuresLookAlike: wrong password, unknown user and disabled
// account all fail the same way and cost one password verification each;
// unknown usernames are verified against the dummy hash.
func TestLoginFailuresLookAlike(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	bob := e.addUser("bob", domain.RoleUser)
	root := e.addUser("root", domain.RoleAdmin)
	if _, err := e.svc.Update(e.ctx(), root.Actor(), bob.ID, users.UpdateParams{Version: 1, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, username, password, verified string
	}{
		{"wrong password", "alice", otherPass, alice.PasswordHash},
		{"unknown user", "carol", goodPass, e.hasher.dummy()},
		{"invalid username", "Not A User!", goodPass, e.hasher.dummy()},
		{"disabled", "bob", goodPass, bob.PasswordHash},
		{"too long password", "alice", goodPass + strings.Repeat("x", 300), alice.PasswordHash},
		{"empty password", "alice", "", alice.PasswordHash},
	}
	for _, tt := range tests {
		before := len(e.hasher.verifications())
		_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: tt.username, Password: tt.password, ClientIP: testIP})
		wantErr(t, tt.name, err, domain.ErrUnauthenticated)
		v := e.hasher.verifications()[before:]
		if len(v) != 1 || v[0] != tt.verified {
			t.Errorf("%s: verified %v, want exactly one verification against %q", tt.name, v, tt.verified)
		}
	}
	if got := e.get(alice.ID); !got.LastLoginAt.IsZero() || got.Version != 1 {
		t.Errorf("failed logins changed the user: %+v", got)
	}
	if n, _ := e.store.DeleteExpiredSessions(e.ctx(), t0().Add(1000*day)); n != 0 {
		t.Errorf("failed logins created %d sessions", n)
	}
	// Unknown usernames are not logged (they are often mistyped passwords).
	if logs := e.logs.String(); strings.Contains(logs, "carol") || strings.Contains(logs, goodPass) || strings.Contains(logs, otherPass) {
		t.Errorf("logs leak credentials: %s", logs)
	}
}

func TestLoginRateLimits(t *testing.T) {
	t.Parallel()
	t.Run("per client ip", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.addUser("alice", domain.RoleUser)
		e.ipLim.setBudget(2)
		for range 2 {
			_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: otherPass, ClientIP: testIP})
			wantErr(t, "wrong password", err, domain.ErrUnauthenticated)
		}
		verified := len(e.hasher.verifications())
		_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: goodPass, ClientIP: testIP})
		wantErr(t, "Login(over the ip limit)", err, domain.ErrRateLimited)
		if len(e.hasher.verifications()) != verified {
			t.Error("a rate-limited login verified the password")
		}
		if _, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: goodPass, ClientIP: "198.51.100.7"}); err != nil {
			t.Errorf("another address: %v", err)
		}
	})
	t.Run("per username", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.addUser("alice", domain.RoleUser)
		e.userLim.setBudget(2)
		for i, ip := range []string{"198.51.100.1", "198.51.100.2"} {
			_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: []string{"alice", "ALICE"}[i], Password: otherPass, ClientIP: ip})
			wantErr(t, "wrong password", err, domain.ErrUnauthenticated)
		}
		_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "Alice", Password: goodPass, ClientIP: "198.51.100.3"})
		wantErr(t, "Login(over the username limit)", err, domain.ErrRateLimited)

		// Unknown usernames have their own buckets; usernames that cannot
		// exist share one.
		for range 2 {
			_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "nobody", Password: goodPass})
			wantErr(t, "unknown user", err, domain.ErrUnauthenticated)
		}
		_, _, err = e.svc.Login(e.ctx(), users.LoginParams{Username: "nobody", Password: goodPass})
		wantErr(t, "unknown user over the limit", err, domain.ErrRateLimited)
		for _, name := range []string{"not valid 1", "not valid 2"} {
			_, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: name, Password: goodPass})
			wantErr(t, "invalid username", err, domain.ErrUnauthenticated)
		}
		_, _, err = e.svc.Login(e.ctx(), users.LoginParams{Username: strings.Repeat("y", 10000), Password: goodPass})
		wantErr(t, "invalid username over the shared limit", err, domain.ErrRateLimited)
		for _, k := range e.userLim.seen() {
			if len(k) > users.MaxUsernameLen {
				t.Errorf("limiter key of %d bytes", len(k))
			}
		}
		if !slices.Contains(e.userLim.seen(), "alice") || slices.Contains(e.userLim.seen(), "ALICE") {
			t.Errorf("username keys are not lower case: %q", e.userLim.seen())
		}
	})
}

func TestLoginRehashesOutdatedHashes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	e.hasher.setParams("v2")
	e.login("alice")
	got := e.get(alice.ID)
	if !strings.HasPrefix(got.PasswordHash, "fake$v2$") || !got.PasswordChangedAt.Equal(alice.PasswordChangedAt) {
		t.Errorf("after login: hash %q, password changed at %v", got.PasswordHash, got.PasswordChangedAt)
	}
	e.login("alice") // the new hash works and is kept
	if again := e.get(alice.ID); again.PasswordHash != got.PasswordHash {
		t.Error("a current hash was replaced")
	}
}

// TestLoginRacingWithAReset: a reset (or disable) that lands between the
// password check and the login record makes the login fail and leaves no
// session behind.
func TestLoginRacingWithAReset(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	racing := &racingUsers{Repository: e.store, onUpdate: func() {
		cur := e.get(alice.ID)
		cur.PasswordHash = "fake$v1$reset"
		if _, err := e.store.UpdateUser(e.ctx(), cur); err != nil {
			t.Error(err)
		}
	}}
	e2 := newEnv(t, func(d *users.Deps, _ *users.Config) {
		d.Users, d.Sessions, d.Tokens = racing, e.store, e.store
	})
	_, _, err := e2.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: goodPass})
	wantErr(t, "Login(racing with a reset)", err, domain.ErrUnauthenticated)
	if n, _ := e.store.DeleteUserSessions(e.ctx(), alice.ID, nil); n != 0 {
		t.Errorf("the failed login left %d sessions", n)
	}
}

// racingUsers runs onUpdate once, before the first UpdateUser.
type racingUsers struct {
	users.Repository
	onUpdate func()
	done     bool
}

func (r *racingUsers) UpdateUser(ctx context.Context, u users.User) (users.User, error) {
	if !r.done {
		r.done = true
		r.onUpdate()
	}
	return r.Repository.UpdateUser(ctx, u)
}

func TestSessionExpiry(t *testing.T) {
	t.Parallel()
	t.Run("idle timeout", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.addUser("alice", domain.RoleUser)
		tok := e.login("alice")
		e.clock.Advance(7*day - time.Second)
		if !e.sessionValid(tok) {
			t.Fatal("session expired before the idle timeout")
		}
		e.clock.Advance(7 * day) // idle for 7 days since the last use
		if e.sessionValid(tok) {
			t.Fatal("idle session still valid")
		}
		if _, err := e.store.GetSession(e.ctx(), users.HashToken(tok)); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the expired session was kept: %v", err)
		}
	})
	t.Run("absolute lifetime", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.addUser("alice", domain.RoleUser)
		tok := e.login("alice")
		for range 4 { // used every 6 days: never idle
			e.clock.Advance(6 * day)
			if !e.sessionValid(tok) {
				t.Fatalf("session expired at %v", e.clock.Now())
			}
		}
		// Day 24: the idle expiry is capped at the absolute one.
		_, s, err := e.svc.AuthenticateSession(e.ctx(), tok)
		if err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(6*day - time.Second) // day 30 minus a second
		if _, s2, err := e.svc.AuthenticateSession(e.ctx(), tok); err != nil || !s2.IdleExpiresAt.Equal(s.ExpiresAt) {
			t.Fatalf("near the end: (%+v, %v), want idle expiry capped at %v", s2, err, s.ExpiresAt)
		}
		e.clock.Advance(time.Second)
		if e.sessionValid(tok) {
			t.Error("session valid after 30 days")
		}
	})
}

func TestSessionTouchIsThrottled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleAdmin)
	tok := e.login("alice")
	stored := func() users.Session {
		s, err := e.store.GetSession(e.ctx(), users.HashToken(tok))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	e.clock.Advance(59 * time.Second)
	actor, s, err := e.svc.AuthenticateSession(e.ctx(), tok)
	if err != nil || actor != alice.Actor() {
		t.Fatalf("AuthenticateSession = (%+v, %v)", actor, err)
	}
	if got := stored(); !got.LastSeenAt.Equal(t0()) || !s.LastSeenAt.Equal(t0()) {
		t.Errorf("touched within a minute: stored %v, returned %v", got.LastSeenAt, s.LastSeenAt)
	}
	at := e.clock.Advance(time.Second)
	if _, s, err = e.svc.AuthenticateSession(e.ctx(), tok); err != nil {
		t.Fatal(err)
	}
	if got := stored(); !got.LastSeenAt.Equal(at) || !got.IdleExpiresAt.Equal(at.Add(7*day)) || !s.IdleExpiresAt.Equal(at.Add(7*day)) {
		t.Errorf("after a minute: stored %+v, returned %+v", got, s)
	}
	// The actor carries the user's current role.
	cur := e.get(alice.ID)
	cur.Role = domain.RoleUser
	if _, err := e.store.UpdateUser(e.ctx(), cur); err != nil {
		t.Fatal(err)
	}
	if actor, _, _ := e.svc.AuthenticateSession(e.ctx(), tok); actor.Role != domain.RoleUser {
		t.Errorf("actor role = %q after a demotion", actor.Role)
	}
}

func TestAuthenticateSessionRejects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	tok := e.login("alice")
	for name, bad := range map[string]string{
		"empty":         "",
		"short":         tok[:42],
		"long":          tok + "A",
		"bad character": tok[:42] + "=",
		"api token":     users.APITokenPrefix + tok,
		"unknown":       strings.Repeat("A", 43),
	} {
		_, _, err := e.svc.AuthenticateSession(e.ctx(), bad)
		wantErr(t, name, err, domain.ErrUnauthenticated)
		if err != nil && bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s: the error contains the token", name)
		}
	}
	// A disabled user's session stops working and is deleted.
	cur := e.get(alice.ID)
	cur.DisabledAt = e.clock.Now()
	if _, err := e.store.UpdateUser(e.ctx(), cur); err != nil {
		t.Fatal(err)
	}
	if e.sessionValid(tok) {
		t.Error("a disabled user's session works")
	}
	if _, err := e.store.GetSession(e.ctx(), users.HashToken(tok)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a disabled user's session was kept: %v", err)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addUser("alice", domain.RoleUser)
	a, b := e.login("alice"), e.login("alice")
	if err := e.svc.Logout(e.ctx(), a); err != nil {
		t.Fatal(err)
	}
	if e.sessionValid(a) || !e.sessionValid(b) {
		t.Error("Logout must end exactly its own session")
	}
	for _, tok := range []string{a, "", "garbage"} {
		if err := e.svc.Logout(e.ctx(), tok); err != nil {
			t.Errorf("Logout(%q) = %v", tok, err)
		}
	}
}

func TestChangePassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	current, other := e.login("alice"), e.login("alice")
	e.clock.Advance(time.Hour)

	err := e.svc.ChangePassword(e.ctx(), alice.Actor(), current, otherPass, "a brand new password")
	wantFields(t, "ChangePassword(wrong current)", err, "current_password")
	err = e.svc.ChangePassword(e.ctx(), alice.Actor(), current, goodPass, "too short")
	wantFields(t, "ChangePassword(short new)", err, "new_password")
	err = e.svc.ChangePassword(e.ctx(), domain.Actor{Role: domain.RoleAdmin}, "", goodPass, "a brand new password")
	wantErr(t, "ChangePassword(no account)", err, domain.ErrPermissionDenied)
	if !e.sessionValid(other) {
		t.Fatal("refused changes revoked a session")
	}

	if err := e.svc.ChangePassword(e.ctx(), alice.Actor(), current, goodPass, "a brand new password"); err != nil {
		t.Fatal(err)
	}
	if !e.sessionValid(current) || e.sessionValid(other) {
		t.Error("ChangePassword must keep the current session and revoke the others")
	}
	got := e.get(alice.ID)
	if !got.PasswordChangedAt.Equal(e.clock.Now()) || !got.UpdatedAt.Equal(e.clock.Now()) {
		t.Errorf("after the change: %+v", got)
	}
	_, _, err = e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: goodPass})
	wantErr(t, "Login(old password)", err, domain.ErrUnauthenticated)
	if _, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: "a brand new password"}); err != nil {
		t.Errorf("Login(new password): %v", err)
	}

	// Without a session token every session ends.
	if err := e.svc.ChangePassword(e.ctx(), alice.Actor(), "", "a brand new password", otherPass); err != nil {
		t.Fatal(err)
	}
	if e.sessionValid(current) {
		t.Error("a change without a session token kept a session")
	}
}

func TestChangePasswordIsRateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	alice := e.addUser("alice", domain.RoleUser)
	e.userLim.setBudget(2)
	for range 2 {
		wantFields(t, "wrong current", e.svc.ChangePassword(e.ctx(), alice.Actor(), "", otherPass, "a brand new password"), "current_password")
	}
	err := e.svc.ChangePassword(e.ctx(), alice.Actor(), "", goodPass, "a brand new password")
	wantErr(t, "ChangePassword(over the limit)", err, domain.ErrRateLimited)
	// The budget is the username's, shared with Login.
	_, _, err = e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: goodPass})
	wantErr(t, "Login after the guesses", err, domain.ErrRateLimited)
}

func TestAdminChangesRevokeSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.addUser("root", domain.RoleAdmin)
	alice := e.addUser("alice", domain.RoleUser)
	admin := root.Actor()
	rootSession := e.login("root")
	tok := e.login("alice")

	// A display name change keeps the sessions.
	u, err := e.svc.Update(e.ctx(), admin, alice.ID, users.UpdateParams{Version: e.get(alice.ID).Version, DisplayName: ptr("Alice")})
	if err != nil || !e.sessionValid(tok) {
		t.Fatalf("display name change: %v, session valid %v", err, e.sessionValid(tok))
	}
	// A role change revokes them.
	if u, err = e.svc.Update(e.ctx(), admin, alice.ID, users.UpdateParams{Version: u.Version, Role: domain.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if e.sessionValid(tok) {
		t.Error("a role change kept the session")
	}
	// So does disabling.
	tok = e.login("alice")
	if _, err = e.svc.Update(e.ctx(), admin, alice.ID, users.UpdateParams{Version: e.get(alice.ID).Version, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if e.sessionValid(tok) {
		t.Error("disabling kept the session")
	}
	_, _, err = e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: goodPass})
	wantErr(t, "Login(disabled)", err, domain.ErrUnauthenticated)
	if _, err = e.svc.Update(e.ctx(), admin, alice.ID, users.UpdateParams{Version: e.get(alice.ID).Version, Disabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	// And a password reset, which also changes the password.
	tok = e.login("alice")
	if _, err := e.svc.ResetPassword(e.ctx(), admin, alice.ID, "short"); err == nil {
		t.Error("ResetPassword accepted a short password")
	} else {
		wantFields(t, "ResetPassword(short)", err, "new_password")
	}
	if _, err := e.svc.ResetPassword(e.ctx(), admin, alice.ID, otherPass); err != nil {
		t.Fatal(err)
	}
	if e.sessionValid(tok) {
		t.Error("a password reset kept the session")
	}
	if _, _, err := e.svc.Login(e.ctx(), users.LoginParams{Username: "alice", Password: otherPass}); err != nil {
		t.Errorf("Login(reset password): %v", err)
	}
	// None of it touched the admin's own session.
	if !e.sessionValid(rootSession) {
		t.Error("the admin's session was revoked")
	}
}
