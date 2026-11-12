package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of users.Repository,
// users.SessionRepository and users.APITokenRepository. Their values hold no
// maps or slices, so there is no aliasing to check.

func newUser(t *testing.T, n int) users.User {
	return users.User{
		ID: userID(t, n), Username: fmt.Sprintf("user%d", n), DisplayName: fmt.Sprintf("User %d", n),
		Role: domain.RoleUser, PasswordHash: fmt.Sprintf("hash-of-user-%d", n),
		CreatedAt: t0(), UpdatedAt: t0(), PasswordChangedAt: t0(), Version: 1,
	}
}

func mustCreateUser(t *testing.T, s Store, u users.User) {
	t.Helper()
	err := s.CreateUser(context.Background(), u)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
}

// requireUsers checks that the users ports work before a users test.
func requireUsers(t *testing.T, s Store) {
	t.Helper()
	_, err := s.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
}

func hash(n int) users.TokenHash { return users.HashToken(fmt.Sprintf("token-%d", n)) }

func testUserCreateGetList(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	bob, alice := newUser(t, 1), newUser(t, 2)
	bob.Username, alice.Username = "bob", "alice"
	bob.LastLoginAt = t0().Add(time.Minute)
	mustCreateUser(t, s, bob)
	mustCreateUser(t, s, alice)

	wantErr(t, "CreateUser(duplicate id)", s.CreateUser(ctx, bob), domain.ErrAlreadyExists)
	dupName := newUser(t, 3)
	dupName.Username = "bob"
	wantErr(t, "CreateUser(duplicate username)", s.CreateUser(ctx, dupName), domain.ErrAlreadyExists)

	if n, err := s.CountUsers(ctx); err != nil || n != 2 {
		t.Errorf("CountUsers = (%d, %v), want 2", n, err)
	}
	got, err := s.GetUser(ctx, bob.ID)
	if err != nil || got.Username != "bob" || got.DisplayName != bob.DisplayName || got.Role != domain.RoleUser ||
		got.PasswordHash != bob.PasswordHash || got.Version != 1 || !got.CreatedAt.Equal(t0()) ||
		!got.LastLoginAt.Equal(t0().Add(time.Minute)) || !got.DisabledAt.IsZero() {
		t.Errorf("GetUser = (%+v, %v)", got, err)
	}
	_, err = s.GetUser(ctx, userID(t, 99))
	wantErr(t, "GetUser(missing)", err, domain.ErrNotFound)

	if got, err := s.GetUserByUsername(ctx, "alice"); err != nil || got.ID != alice.ID || !got.LastLoginAt.IsZero() {
		t.Errorf("GetUserByUsername = (%+v, %v)", got, err)
	}
	_, err = s.GetUserByUsername(ctx, "carol")
	wantErr(t, "GetUserByUsername(missing)", err, domain.ErrNotFound)

	list, err := s.ListUsers(ctx)
	if err != nil || len(list) != 2 || list[0].ID != alice.ID || list[1].ID != bob.ID {
		t.Errorf("ListUsers = (%v, %v), want by username", list, err)
	}

	admin, disabledAdmin := newUser(t, 4), newUser(t, 5)
	admin.Role, disabledAdmin.Role = domain.RoleAdmin, domain.RoleAdmin
	disabledAdmin.DisabledAt = t0()
	mustCreateUser(t, s, admin)
	mustCreateUser(t, s, disabledAdmin)
	if n, err := s.CountActiveAdmins(ctx); err != nil || n != 1 {
		t.Errorf("CountActiveAdmins = (%d, %v), want 1", n, err)
	}
}

func testUserUpdateVersioned(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	u := newUser(t, 1)
	mustCreateUser(t, s, u)

	next := u
	next.DisplayName, next.Role, next.PasswordHash = "New Name", domain.RoleAdmin, "new-hash"
	next.DisabledAt, next.PasswordChangedAt = t0().Add(time.Hour), t0().Add(2*time.Hour)
	next.LastLoginAt, next.UpdatedAt = t0().Add(3*time.Hour), t0().Add(4*time.Hour)
	// Immutable fields are never written.
	next.Username, next.CreatedAt = "renamed", t0().Add(time.Minute)
	stored, err := s.UpdateUser(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.DisplayName != "New Name" || stored.Role != domain.RoleAdmin || stored.PasswordHash != "new-hash" ||
		!stored.DisabledAt.Equal(t0().Add(time.Hour)) || !stored.PasswordChangedAt.Equal(t0().Add(2*time.Hour)) ||
		!stored.LastLoginAt.Equal(t0().Add(3*time.Hour)) || !stored.UpdatedAt.Equal(t0().Add(4*time.Hour)) {
		t.Errorf("UpdateUser returned %+v", stored)
	}
	if stored.Username != u.Username || !stored.CreatedAt.Equal(t0()) {
		t.Errorf("UpdateUser wrote immutable fields: %+v", stored)
	}
	if got, _ := s.GetUser(ctx, u.ID); got != stored {
		t.Errorf("GetUser after update = %+v, want %+v", got, stored)
	}
	// Zero times are stored as zero again.
	cleared := stored
	cleared.DisabledAt, cleared.LastLoginAt = time.Time{}, time.Time{}
	if got, err := s.UpdateUser(ctx, cleared); err != nil || !got.DisabledAt.IsZero() || !got.LastLoginAt.IsZero() || got.Version != 3 {
		t.Errorf("UpdateUser(clear times) = (%+v, %v)", got, err)
	}

	_, err = s.UpdateUser(ctx, u) // version 1
	wantErr(t, "UpdateUser(stale version)", err, domain.ErrConflict)
	_, err = s.UpdateUser(ctx, newUser(t, 9))
	wantErr(t, "UpdateUser(missing)", err, domain.ErrNotFound)

	// Concurrent updates of one version: exactly one wins.
	cur, _ := s.GetUser(ctx, u.ID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 20 {
		wg.Go(func() {
			next := cur
			next.DisplayName = fmt.Sprintf("racer %d", i)
			if _, err := s.UpdateUser(ctx, next); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent updates of one version succeeded, want 1", wins)
	}
}

func testUserFirstUserRace(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			u := newUser(t, i+1)
			u.Role = domain.RoleAdmin
			errs[i] = s.CreateFirstUser(ctx, u)
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, domain.ErrFailedPrecondition):
			t.Errorf("CreateFirstUser: %v, want nil or ErrFailedPrecondition", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d concurrent CreateFirstUser calls succeeded, want exactly 1", wins)
	}
	if c, err := s.CountUsers(ctx); err != nil || c != 1 {
		t.Errorf("CountUsers = (%d, %v), want 1", c, err)
	}
	wantErr(t, "CreateFirstUser(users exist)", s.CreateFirstUser(ctx, newUser(t, 99)), domain.ErrFailedPrecondition)
	// CreateUser is not restricted.
	mustCreateUser(t, s, newUser(t, 50))
}

func testUserDelete(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	u, other := newUser(t, 1), newUser(t, 2)
	mustCreateUser(t, s, u)
	mustCreateUser(t, s, other)
	sess := users.Session{IDHash: hash(1), UserID: u.ID, CreatedAt: t0(), LastSeenAt: t0(), IdleExpiresAt: t0().Add(time.Hour), ExpiresAt: t0().Add(24 * time.Hour)}
	tok := users.APIToken{ID: domain.NewAPITokenID(), UserID: u.ID, Name: "ci", Prefix: "syncr_pat_abcd", Hash: hash(2), CreatedAt: t0()}
	keepSess := sess
	keepSess.IDHash, keepSess.UserID = hash(3), other.ID
	for _, err := range []error{s.CreateSession(ctx, sess), s.CreateAPIToken(ctx, tok), s.CreateSession(ctx, keepSess)} {
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	_, err := s.GetUser(ctx, u.ID)
	wantErr(t, "GetUser(deleted)", err, domain.ErrNotFound)
	_, err = s.GetSession(ctx, sess.IDHash)
	wantErr(t, "GetSession(of deleted user)", err, domain.ErrNotFound)
	_, err = s.GetAPITokenByHash(ctx, tok.Hash)
	wantErr(t, "GetAPITokenByHash(of deleted user)", err, domain.ErrNotFound)
	if _, err := s.GetSession(ctx, keepSess.IDHash); err != nil {
		t.Errorf("another user's session was deleted: %v", err)
	}
	wantErr(t, "DeleteUser(deleted)", s.DeleteUser(ctx, u.ID), domain.ErrNotFound)
	// The username is free again.
	again := newUser(t, 3)
	again.Username = u.Username
	mustCreateUser(t, s, again)
}

func testUserDeleteOwner(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	t.Run("jobs", func(t *testing.T) {
		w := newWorker(t, 1)
		mustCreateWorker(t, s, w)
		mustCreateJob(t, s, newJob(t, 1, w.ID))
		wantErr(t, "DeleteUser(owns a job)", s.DeleteUser(ctx, ownerID(t)), domain.ErrFailedPrecondition)
		if _, err := s.GetUser(ctx, ownerID(t)); err != nil {
			t.Errorf("the job owner was deleted: %v", err)
		}
	})
	t.Run("remotes", func(t *testing.T) {
		u := newUser(t, 10)
		mustCreateUser(t, s, u)
		r := newRemote(t, 1, u.ID, "Photos")
		mustCreateRemote(t, s, r)
		if err := s.DeleteRemote(ctx, r.ID, t0()); err != nil {
			t.Fatal(err)
		}
		// Deleted remotes still count: job history refers to them.
		wantErr(t, "DeleteUser(owns a deleted remote)", s.DeleteUser(ctx, u.ID), domain.ErrFailedPrecondition)
	})
}

func testSessions(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	u, other := newUser(t, 1), newUser(t, 2)
	mustCreateUser(t, s, u)
	mustCreateUser(t, s, other)
	newSession := func(n int, user domain.UserID, idle, abs time.Duration) users.Session {
		return users.Session{
			IDHash: hash(n), UserID: user, CreatedAt: t0(), LastSeenAt: t0(),
			IdleExpiresAt: t0().Add(idle), ExpiresAt: t0().Add(abs), UserAgent: "test-agent", ClientIP: "192.0.2.1",
		}
	}
	s1, s2, s3 := newSession(1, u.ID, time.Hour, 24*time.Hour), newSession(2, u.ID, time.Hour, 24*time.Hour), newSession(3, other.ID, time.Hour, 24*time.Hour)
	for _, sess := range []users.Session{s1, s2, s3} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	wantErr(t, "CreateSession(duplicate)", s.CreateSession(ctx, s1), domain.ErrAlreadyExists)
	wantErr(t, "CreateSession(unknown user)", s.CreateSession(ctx, newSession(4, userID(t, 99), time.Hour, time.Hour)), domain.ErrNotFound)

	got, err := s.GetSession(ctx, s1.IDHash)
	if err != nil || got.IDHash != s1.IDHash || got.UserID != s1.UserID || !got.CreatedAt.Equal(s1.CreatedAt) ||
		!got.LastSeenAt.Equal(s1.LastSeenAt) || !got.IdleExpiresAt.Equal(s1.IdleExpiresAt) || !got.ExpiresAt.Equal(s1.ExpiresAt) ||
		got.UserAgent != s1.UserAgent || got.ClientIP != s1.ClientIP {
		t.Errorf("GetSession = (%+v, %v)", got, err)
	}
	_, err = s.GetSession(ctx, hash(99))
	wantErr(t, "GetSession(missing)", err, domain.ErrNotFound)

	if err := s.TouchSession(ctx, s1.IDHash, t0().Add(time.Minute), t0().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, s1.IDHash); !got.LastSeenAt.Equal(t0().Add(time.Minute)) || !got.IdleExpiresAt.Equal(t0().Add(2*time.Hour)) || !got.ExpiresAt.Equal(s1.ExpiresAt) {
		t.Errorf("after TouchSession: %+v", got)
	}
	wantErr(t, "TouchSession(missing)", s.TouchSession(ctx, hash(99), t0(), t0()), domain.ErrNotFound)

	// Revoke every session of u but s1.
	keep := s1.IDHash
	if n, err := s.DeleteUserSessions(ctx, u.ID, &keep); err != nil || n != 1 {
		t.Errorf("DeleteUserSessions(keep s1) = (%d, %v), want 1", n, err)
	}
	if _, err := s.GetSession(ctx, s1.IDHash); err != nil {
		t.Errorf("the kept session is gone: %v", err)
	}
	_, err = s.GetSession(ctx, s2.IDHash)
	wantErr(t, "GetSession(revoked)", err, domain.ErrNotFound)

	// Expiry: idle or absolute.
	idleGone, absGone := newSession(5, u.ID, -time.Second, time.Hour), newSession(6, u.ID, time.Hour, 0)
	for _, sess := range []users.Session{idleGone, absGone} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DeleteExpiredSessions(ctx, t0()); err != nil || n != 2 {
		t.Errorf("DeleteExpiredSessions = (%d, %v), want 2", n, err)
	}
	if _, err := s.GetSession(ctx, s3.IDHash); err != nil {
		t.Errorf("a live session expired: %v", err)
	}

	if err := s.DeleteSession(ctx, s1.IDHash); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetSession(ctx, s1.IDHash)
	wantErr(t, "GetSession(deleted)", err, domain.ErrNotFound)
	if err := s.DeleteSession(ctx, s1.IDHash); err != nil {
		t.Errorf("DeleteSession(missing) = %v, want nil", err)
	}
	if n, err := s.DeleteUserSessions(ctx, other.ID, nil); err != nil || n != 1 {
		t.Errorf("DeleteUserSessions(all) = (%d, %v), want 1", n, err)
	}
}

func testAPITokens(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	u, other := newUser(t, 1), newUser(t, 2)
	mustCreateUser(t, s, u)
	mustCreateUser(t, s, other)
	tokenID := func(n int) domain.APITokenID { return id(t, domain.ParseAPITokenID, 4000+n) }
	newToken := func(n int, user domain.UserID) users.APIToken {
		return users.APIToken{ID: tokenID(n), UserID: user, Name: fmt.Sprintf("token %d", n), Prefix: "syncr_pat_" + fmt.Sprint(1000+n), Hash: hash(n), CreatedAt: t0()}
	}
	t1, t2, t3 := newToken(1, u.ID), newToken(2, u.ID), newToken(3, other.ID)
	t2.ExpiresAt = t0().Add(24 * time.Hour)
	for _, tok := range []users.APIToken{t1, t2, t3} {
		if err := s.CreateAPIToken(ctx, tok); err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
	}
	dupHash := newToken(4, u.ID)
	dupHash.Hash = t1.Hash
	wantErr(t, "CreateAPIToken(duplicate id)", s.CreateAPIToken(ctx, t1), domain.ErrAlreadyExists)
	wantErr(t, "CreateAPIToken(duplicate hash)", s.CreateAPIToken(ctx, dupHash), domain.ErrAlreadyExists)
	wantErr(t, "CreateAPIToken(unknown user)", s.CreateAPIToken(ctx, newToken(5, userID(t, 99))), domain.ErrNotFound)

	got, err := s.GetAPITokenByHash(ctx, t2.Hash)
	if err != nil || got.ID != t2.ID || got.UserID != u.ID || got.Name != t2.Name || got.Prefix != t2.Prefix ||
		!got.ExpiresAt.Equal(t2.ExpiresAt) || !got.LastUsedAt.IsZero() || !got.CreatedAt.Equal(t0()) {
		t.Errorf("GetAPITokenByHash = (%+v, %v)", got, err)
	}
	if got, _ := s.GetAPITokenByHash(ctx, t1.Hash); !got.ExpiresAt.IsZero() {
		t.Errorf("a token without expiry came back with %v", got.ExpiresAt)
	}
	_, err = s.GetAPITokenByHash(ctx, hash(99))
	wantErr(t, "GetAPITokenByHash(missing)", err, domain.ErrNotFound)

	list, err := s.ListAPITokens(ctx, u.ID)
	if err != nil || len(list) != 2 || list[0].ID != t2.ID || list[1].ID != t1.ID {
		t.Errorf("ListAPITokens = (%v, %v), want newest first", list, err)
	}
	if list, err := s.ListAPITokens(ctx, userID(t, 99)); err != nil || len(list) != 0 {
		t.Errorf("ListAPITokens(nobody) = (%v, %v)", list, err)
	}

	if err := s.TouchAPIToken(ctx, t1.ID, t0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAPITokenByHash(ctx, t1.Hash); !got.LastUsedAt.Equal(t0().Add(time.Minute)) {
		t.Errorf("LastUsedAt = %v after TouchAPIToken", got.LastUsedAt)
	}
	wantErr(t, "TouchAPIToken(missing)", s.TouchAPIToken(ctx, tokenID(99), t0()), domain.ErrNotFound)

	wantErr(t, "DeleteAPIToken(another user's)", s.DeleteAPIToken(ctx, other.ID, t1.ID), domain.ErrNotFound)
	if err := s.DeleteAPIToken(ctx, u.ID, t1.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "DeleteAPIToken(deleted)", s.DeleteAPIToken(ctx, u.ID, t1.ID), domain.ErrNotFound)
	_, err = s.GetAPITokenByHash(ctx, t1.Hash)
	wantErr(t, "GetAPITokenByHash(deleted)", err, domain.ErrNotFound)
}

func testAPITokenPurge(t *testing.T, s Store) {
	requireUsers(t, s)
	ctx := context.Background()
	u, other := newUser(t, 1), newUser(t, 2)
	mustCreateUser(t, s, u)
	mustCreateUser(t, s, other)
	tokenID := func(n int) domain.APITokenID { return id(t, domain.ParseAPITokenID, 4000+n) }
	newToken := func(n int, user domain.UserID, expires time.Time) users.APIToken {
		return users.APIToken{ID: tokenID(n), UserID: user, Name: fmt.Sprintf("token %d", n), Prefix: "syncr_pat_" + fmt.Sprint(1000+n),
			Hash: hash(n), CreatedAt: t0(), ExpiresAt: expires}
	}
	now := t0().Add(48 * time.Hour)
	tokens := []users.APIToken{
		newToken(1, u.ID, time.Time{}),             // never expires
		newToken(2, u.ID, now.Add(-time.Hour)),     // expired
		newToken(3, u.ID, now),                     // expires now: expired
		newToken(4, other.ID, now.Add(time.Hour)),  // valid
		newToken(5, other.ID, now.Add(-time.Hour)), // expired
	}
	for _, tok := range tokens {
		if err := s.CreateAPIToken(ctx, tok); err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
	}
	n, err := s.DeleteExpiredAPITokens(ctx, now)
	skipIfUnsupported(t, "DeleteExpiredAPITokens", "track C (users)", err)
	if n != 3 {
		t.Errorf("DeleteExpiredAPITokens = %d, want 3", n)
	}
	for _, tok := range tokens {
		_, err := s.GetAPITokenByHash(ctx, tok.Hash)
		if expired := tok.Expired(now); expired != errorsIs(err, domain.ErrNotFound) {
			t.Errorf("token %s (expired %v): GetAPITokenByHash err = %v", tok.Name, expired, err)
		}
	}
	n, err = s.DeleteUserAPITokens(ctx, other.ID)
	if err != nil || n != 1 {
		t.Errorf("DeleteUserAPITokens = (%d, %v), want 1", n, err)
	}
	if left, _ := s.ListAPITokens(ctx, other.ID); len(left) != 0 {
		t.Errorf("tokens left for the user: %v", left)
	}
	if left, _ := s.ListAPITokens(ctx, u.ID); len(left) != 1 || left[0].ID != tokenID(1) {
		t.Errorf("tokens of the other user: %v", left)
	}
	if n, err := s.DeleteUserAPITokens(ctx, userID(t, 99)); err != nil || n != 0 {
		t.Errorf("DeleteUserAPITokens(unknown user) = (%d, %v)", n, err)
	}
}
