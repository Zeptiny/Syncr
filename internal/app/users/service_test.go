package users_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// fakeHasher is a fast, deterministic users.PasswordHasher:
// "fake$<params>$<hex sha256(password)>". Changing params makes older
// hashes report needsRehash. It records what it verified.
type fakeHasher struct {
	mu       sync.Mutex
	params   string
	hashed   []string
	verified []string
}

func (h *fakeHasher) setParams(p string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.params = p
}

func (h *fakeHasher) Hash(_ context.Context, password string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sum := sha256.Sum256([]byte(password))
	enc := "fake$" + h.params + "$" + hex.EncodeToString(sum[:])
	h.hashed = append(h.hashed, enc)
	return enc, nil
}

func (h *fakeHasher) Verify(_ context.Context, password, encoded string) (ok, needsRehash bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.verified = append(h.verified, encoded)
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "fake" {
		return false, false, errors.New("malformed fake hash")
	}
	sum := sha256.Sum256([]byte(password))
	ok = subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(parts[2])) == 1
	return ok, parts[1] != h.params, nil
}

// verifications returns the encoded hashes verified so far.
func (h *fakeHasher) verifications() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.verified...)
}

// dummy returns the first hash made: New's dummy hash.
func (h *fakeHasher) dummy() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hashed[0]
}

// budgetLimiter allows budget calls per key (0: unlimited) and records the
// keys it saw.
type budgetLimiter struct {
	mu     sync.Mutex
	budget int
	used   map[string]int
	keys   []string
}

func (l *budgetLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	if l.budget == 0 {
		return true
	}
	if l.used == nil {
		l.used = map[string]int{}
	}
	if l.used[key] >= l.budget {
		return false
	}
	l.used[key]++
	return true
}

func (l *budgetLimiter) setBudget(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.budget, l.used = n, nil
}

func (l *budgetLimiter) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t        *testing.T
	svc      *users.Service
	store    *memstore.Store
	clock    *clock.Fake
	hasher   *fakeHasher
	ipLim    *budgetLimiter
	userLim  *budgetLimiter
	setupLim *budgetLimiter
	logs     *syncBuffer
}

func randReader() io.Reader { return rand.Reader }

func t0() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) }

const (
	day       = 24 * time.Hour
	goodPass  = "correct horse battery"
	otherPass = "another long password"
	testIP    = "192.0.2.10"
	testAgent = "test-agent/1.0"
	setupCode = "ABCDE-FGHIJ-KLMNO-PQRS2"
)

func newEnv(t *testing.T, mutate ...func(*users.Deps, *users.Config)) *env {
	t.Helper()
	e := &env{
		t: t, store: memstore.New(), clock: clock.NewFake(t0()), hasher: &fakeHasher{params: "v1"},
		ipLim: &budgetLimiter{}, userLim: &budgetLimiter{}, setupLim: &budgetLimiter{}, logs: &syncBuffer{},
	}
	deps := users.Deps{
		Users: e.store, Sessions: e.store, Tokens: e.store, Hasher: e.hasher,
		Limiters: users.Limiters{ClientIP: e.ipLim, Username: e.userLim, Setup: e.setupLim},
		Clock:    e.clock, IDs: &idgen.Sequential{}, Random: rand.Reader,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	var cfg users.Config
	for _, m := range mutate {
		m(&deps, &cfg)
	}
	svc, err := users.New(deps, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *env) ctx() context.Context { return e.t.Context() }

// addUser stores a user with password goodPass.
func (e *env) addUser(username string, role domain.Role) users.User {
	e.t.Helper()
	hash, _ := e.hasher.Hash(e.ctx(), goodPass)
	now := e.clock.Now()
	u := users.User{
		ID: domain.NewUserID(), Username: username, DisplayName: strings.ToUpper(username), Role: role, PasswordHash: hash,
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now, Version: 1,
	}
	if err := e.store.CreateUser(e.ctx(), u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) get(id domain.UserID) users.User {
	e.t.Helper()
	u, err := e.store.GetUser(e.ctx(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

// login logs username in with goodPass and returns the session token.
func (e *env) login(username string) string {
	e.t.Helper()
	_, s, err := e.svc.Login(e.ctx(), users.LoginParams{Username: username, Password: goodPass, ClientIP: testIP, UserAgent: testAgent})
	if err != nil {
		e.t.Fatalf("Login(%s): %v", username, err)
	}
	return s.Token
}

// sessionValid reports whether token authenticates.
func (e *env) sessionValid(token string) bool {
	e.t.Helper()
	_, _, err := e.svc.AuthenticateSession(e.ctx(), token)
	if err != nil && !errors.Is(err, domain.ErrUnauthenticated) {
		e.t.Fatalf("AuthenticateSession: %v", err)
	}
	return err == nil
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s = %v, want %v", what, err, want)
	}
}

// wantFields asserts err is a *domain.ValidationError naming exactly
// fields.
func wantFields(t *testing.T, what string, err error, fields ...string) {
	t.Helper()
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		t.Errorf("%s = %v, want a validation error on %v", what, err, fields)
		return
	}
	var got []string
	for _, f := range verr.Fields {
		got = append(got, f.Field)
	}
	if fmt.Sprint(got) != fmt.Sprint(fields) {
		t.Errorf("%s: fields %v (%v), want %v", what, got, err, fields)
	}
}

func TestNewChecksDependenciesAndConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if len(e.hasher.hashed) != 1 {
		t.Errorf("New hashed %d passwords, want 1 (the dummy)", len(e.hasher.hashed))
	}
	base := func() users.Deps {
		lim := &budgetLimiter{}
		return users.Deps{
			Users: e.store, Sessions: e.store, Tokens: e.store, Hasher: e.hasher,
			Limiters: users.Limiters{ClientIP: lim, Username: lim, Setup: lim},
			Clock:    e.clock, IDs: &idgen.Sequential{}, Random: rand.Reader,
		}
	}
	for name, mutate := range map[string]func(*users.Deps){
		"users":    func(d *users.Deps) { d.Users = nil },
		"sessions": func(d *users.Deps) { d.Sessions = nil },
		"tokens":   func(d *users.Deps) { d.Tokens = nil },
		"hasher":   func(d *users.Deps) { d.Hasher = nil },
		"clock":    func(d *users.Deps) { d.Clock = nil },
		"ids":      func(d *users.Deps) { d.IDs = nil },
		"random":   func(d *users.Deps) { d.Random = nil },
		"ip limit": func(d *users.Deps) { d.Limiters.ClientIP = nil },
		"username": func(d *users.Deps) { d.Limiters.Username = nil },
		"setup":    func(d *users.Deps) { d.Limiters.Setup = nil },
	} {
		d := base()
		mutate(&d)
		if _, err := users.New(d, users.Config{}); err == nil {
			t.Errorf("New without %s succeeded", name)
		}
	}
	for _, cfg := range []users.Config{
		{SessionIdleTimeout: time.Second},
		{SessionMaxAge: time.Second},
		{TouchInterval: -time.Second},
		{MaxAPITokens: -1},
	} {
		if _, err := users.New(base(), cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestMe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u := e.addUser("alice", domain.RoleUser)
	got, err := e.svc.Me(e.ctx(), u.Actor())
	if err != nil || got.ID != u.ID {
		t.Errorf("Me = (%+v, %v)", got, err)
	}
	_, err = e.svc.Me(e.ctx(), domain.Actor{Role: domain.RoleAdmin})
	wantErr(t, "Me(no account)", err, domain.ErrNotFound)
}

func TestAdminUseCasesNeedTheAdminRole(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u := e.addUser("alice", domain.RoleUser)
	for _, actor := range []domain.Actor{u.Actor(), {}} {
		_, err := e.svc.List(e.ctx(), actor)
		wantErr(t, "List", err, domain.ErrPermissionDenied)
		_, err = e.svc.Get(e.ctx(), actor, u.ID)
		wantErr(t, "Get", err, domain.ErrPermissionDenied)
		_, err = e.svc.Create(e.ctx(), actor, users.CreateParams{Username: "bob", Password: goodPass, Role: domain.RoleAdmin})
		wantErr(t, "Create", err, domain.ErrPermissionDenied)
		admin := domain.RoleAdmin
		_, err = e.svc.Update(e.ctx(), actor, u.ID, users.UpdateParams{Version: 1, Role: admin})
		wantErr(t, "Update", err, domain.ErrPermissionDenied)
		_, err = e.svc.ResetPassword(e.ctx(), actor, u.ID, otherPass)
		wantErr(t, "ResetPassword", err, domain.ErrPermissionDenied)
		wantErr(t, "Delete", e.svc.Delete(e.ctx(), actor, u.ID), domain.ErrPermissionDenied)
	}
	if got := e.get(u.ID); got.Role != domain.RoleUser || got.Version != 1 {
		t.Errorf("a refused call changed the user: %+v", got)
	}
}

func TestAdminCreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := e.addUser("root", domain.RoleAdmin).Actor()
	tests := []struct {
		name   string
		p      users.CreateParams
		fields []string
	}{
		{"empty", users.CreateParams{}, []string{"username", "password", "role"}},
		{"upper case", users.CreateParams{Username: "Bob", Password: goodPass, Role: domain.RoleUser}, []string{"username"}},
		{"leading dot", users.CreateParams{Username: ".bob", Password: goodPass, Role: domain.RoleUser}, []string{"username"}},
		{"space", users.CreateParams{Username: "bob smith", Password: goodPass, Role: domain.RoleUser}, []string{"username"}},
		{"too long", users.CreateParams{Username: strings.Repeat("a", 65), Password: goodPass, Role: domain.RoleUser}, []string{"username"}},
		{"short password", users.CreateParams{Username: "bob", Password: "elevenchars", Role: domain.RoleUser}, []string{"password"}},
		{"long password", users.CreateParams{Username: "bob", Password: strings.Repeat("p", 257), Role: domain.RoleUser}, []string{"password"}},
		{"bad role", users.CreateParams{Username: "bob", Password: goodPass, Role: "root"}, []string{"role"}},
		{"control in display name", users.CreateParams{Username: "bob", DisplayName: "Bob\x00", Password: goodPass, Role: domain.RoleUser}, []string{"display_name"}},
		{"long display name", users.CreateParams{Username: "bob", DisplayName: strings.Repeat("é", 129), Password: goodPass, Role: domain.RoleUser}, []string{"display_name"}},
	}
	for _, tt := range tests {
		_, err := e.svc.Create(e.ctx(), admin, tt.p)
		wantFields(t, tt.name, err, tt.fields...)
		if err != nil && strings.Contains(err.Error(), goodPass) {
			t.Errorf("%s: the error contains the password: %v", tt.name, err)
		}
	}

	// Limits are inclusive; passwords count characters, not bytes.
	u, err := e.svc.Create(e.ctx(), admin, users.CreateParams{
		Username: "bob.smith_2-x", DisplayName: "  Bob Smith  ", Password: strings.Repeat("ü", 12), Role: domain.RoleUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.DisplayName != "Bob Smith" || u.Role != domain.RoleUser || u.Version != 1 || !u.CreatedAt.Equal(t0()) || !u.LastLoginAt.IsZero() {
		t.Errorf("created %+v", u)
	}
	if u.PasswordHash == strings.Repeat("ü", 12) || !strings.HasPrefix(u.PasswordHash, "fake$") {
		t.Errorf("password stored as %q", u.PasswordHash)
	}
	if _, err := e.svc.Create(e.ctx(), admin, users.CreateParams{Username: strings.Repeat("a", 64), Password: strings.Repeat("p", 256), Role: domain.RoleAdmin}); err != nil {
		t.Errorf("maximum lengths: %v", err)
	}
	_, err = e.svc.Create(e.ctx(), admin, users.CreateParams{Username: "bob.smith_2-x", Password: goodPass, Role: domain.RoleUser})
	wantErr(t, "Create(duplicate)", err, domain.ErrAlreadyExists)

	list, err := e.svc.List(e.ctx(), admin)
	if err != nil || len(list) != 3 || list[0].Username != strings.Repeat("a", 64) || list[1].Username != "bob.smith_2-x" {
		t.Errorf("List = (%v, %v)", list, err)
	}
	got, err := e.svc.Get(e.ctx(), admin, u.ID)
	if err != nil || got.ID != u.ID {
		t.Errorf("Get = (%+v, %v)", got, err)
	}
	_, err = e.svc.Get(e.ctx(), admin, domain.NewUserID())
	wantErr(t, "Get(missing)", err, domain.ErrNotFound)
	_, err = e.svc.Get(e.ctx(), admin, domain.UserID{})
	wantErr(t, "Get(zero)", err, domain.ErrInvalidID)
}

func ptr[T any](v T) *T { return &v }

func TestAdminUpdate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := e.addUser("root", domain.RoleAdmin).Actor()
	u := e.addUser("alice", domain.RoleUser)

	_, err := e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 7, DisplayName: ptr("x")})
	wantErr(t, "Update(stale version)", err, domain.ErrConflict)
	_, err = e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 1, Role: "root"})
	wantFields(t, "Update(bad role)", err, "role")
	_, err = e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 1, DisplayName: ptr("bad\nname")})
	wantFields(t, "Update(bad display name)", err, "display_name")
	_, err = e.svc.Update(e.ctx(), admin, domain.NewUserID(), users.UpdateParams{Version: 1})
	wantErr(t, "Update(missing)", err, domain.ErrNotFound)

	// No change: nothing is written.
	same, err := e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 1, DisplayName: ptr(u.DisplayName), Role: u.Role, Disabled: ptr(false)})
	if err != nil || same.Version != 1 {
		t.Errorf("no-op Update = (%+v, %v)", same, err)
	}

	e.clock.Advance(time.Minute)
	got, err := e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 1, DisplayName: ptr(" Alice A. ")})
	if err != nil || got.DisplayName != "Alice A." || got.Version != 2 || !got.UpdatedAt.Equal(t0().Add(time.Minute)) {
		t.Errorf("Update(display name) = (%+v, %v)", got, err)
	}
	got, err = e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 2, Role: domain.RoleAdmin, Disabled: ptr(true)})
	if err != nil || got.Role != domain.RoleAdmin || !got.DisabledAt.Equal(e.clock.Now()) || got.Version != 3 {
		t.Errorf("Update(role, disable) = (%+v, %v)", got, err)
	}
	got, err = e.svc.Update(e.ctx(), admin, u.ID, users.UpdateParams{Version: 3, Disabled: ptr(false)})
	if err != nil || got.Disabled() {
		t.Errorf("Update(enable) = (%+v, %v)", got, err)
	}
}

func TestLastActiveAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.addUser("root", domain.RoleAdmin)
	// A disabled admin does not count.
	off := e.addUser("off", domain.RoleAdmin)
	if _, err := e.svc.Update(e.ctx(), root.Actor(), off.ID, users.UpdateParams{Version: 1, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}

	_, err := e.svc.Update(e.ctx(), root.Actor(), root.ID, users.UpdateParams{Version: 1, Role: domain.RoleUser})
	wantErr(t, "demote the last admin", err, domain.ErrFailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), "last active admin") {
		t.Errorf("message: %v", err)
	}
	_, err = e.svc.Update(e.ctx(), root.Actor(), root.ID, users.UpdateParams{Version: 1, Disabled: ptr(true)})
	wantErr(t, "disable the last admin", err, domain.ErrFailedPrecondition)
	wantErr(t, "delete the last admin", e.svc.Delete(e.ctx(), root.Actor(), root.ID), domain.ErrFailedPrecondition)
	if got := e.get(root.ID); got.Role != domain.RoleAdmin || got.Disabled() || got.Version != 1 {
		t.Errorf("the last admin changed: %+v", got)
	}
	// The other admin is not the last one while disabled: it may be deleted
	// or demoted freely.
	if _, err := e.svc.Update(e.ctx(), root.Actor(), off.ID, users.UpdateParams{Version: 2, Role: domain.RoleUser}); err != nil {
		t.Errorf("demote a disabled admin: %v", err)
	}

	// With a second active admin, either may go, but not both.
	second := e.addUser("second", domain.RoleAdmin)
	if _, err := e.svc.Update(e.ctx(), second.Actor(), root.ID, users.UpdateParams{Version: 1, Role: domain.RoleUser}); err != nil {
		t.Fatalf("demote one of two admins: %v", err)
	}
	wantErr(t, "delete the remaining admin", e.svc.Delete(e.ctx(), second.Actor(), second.ID), domain.ErrFailedPrecondition)
}

func TestConcurrentAdminRemovalsKeepAnAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.addUser("a", domain.RoleAdmin)
	b := e.addUser("b", domain.RoleAdmin)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	wg.Go(func() {
		_, errs[0] = e.svc.Update(e.ctx(), a.Actor(), b.ID, users.UpdateParams{Version: 1, Role: domain.RoleUser})
	})
	wg.Go(func() {
		_, errs[1] = e.svc.Update(e.ctx(), b.Actor(), a.ID, users.UpdateParams{Version: 1, Disabled: ptr(true)})
	})
	wg.Go(func() { errs[2] = e.svc.Delete(e.ctx(), a.Actor(), a.ID) })
	wg.Go(func() { errs[3] = e.svc.Delete(e.ctx(), b.Actor(), b.ID) })
	wg.Wait()
	// Whatever the order, exactly one admin is left: the first removal
	// wins and the survivor is protected (a removal of the one already
	// demoted may succeed too).
	if n, err := e.store.CountActiveAdmins(e.ctx()); err != nil || n != 1 {
		t.Errorf("%d active admins after concurrent removals (%v), want 1", n, errs)
	}
	refused := 0
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, domain.ErrFailedPrecondition):
			refused++
		case !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrConflict):
			t.Errorf("unexpected error %v", err)
		}
	}
	if refused == 0 {
		t.Errorf("no removal was refused: %v", errs)
	}
}

func TestDeleteUser(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.addUser("root", domain.RoleAdmin)
	u := e.addUser("alice", domain.RoleUser)
	token := e.login("alice")
	if _, _, err := e.svc.CreateAPIToken(e.ctx(), u.Actor(), "ci", 0); err != nil {
		t.Fatal(err)
	}

	// A job owner cannot be deleted; the message names no constraint.
	owner := e.addUser("owner", domain.RoleUser)
	w := workers.Worker{ID: domain.NewWorkerID(), Name: "w", Mode: domain.WorkerActive, CreatedAt: t0()}
	if err := e.store.CreateWorker(e.ctx(), w); err != nil {
		t.Fatal(err)
	}
	j := jobs.Job{
		ID: domain.NewJobID(), WorkerID: w.ID, OwnerID: owner.ID, Kind: "rclone_transfer", Status: domain.JobQueued,
		Definition: []byte(`{}`), CreatedAt: t0(), QueuedAt: t0(), ExitCode: -1, Version: 1,
	}
	if err := e.store.CreateJob(e.ctx(), j); err != nil {
		t.Fatal(err)
	}
	err := e.svc.Delete(e.ctx(), root.Actor(), owner.ID)
	wantErr(t, "Delete(job owner)", err, domain.ErrFailedPrecondition)
	if err != nil && (!strings.Contains(err.Error(), "disable the account instead") || strings.Contains(err.Error(), "jobs ")) {
		t.Errorf("message: %v", err)
	}

	if err := e.svc.Delete(e.ctx(), root.Actor(), u.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.GetUser(e.ctx(), u.ID)
	wantErr(t, "GetUser(deleted)", err, domain.ErrNotFound)
	if e.sessionValid(token) {
		t.Error("a deleted user's session still works")
	}
	if ts, _ := e.store.ListAPITokens(e.ctx(), u.ID); len(ts) != 0 {
		t.Errorf("a deleted user's tokens remain: %v", ts)
	}
	wantErr(t, "Delete(again)", e.svc.Delete(e.ctx(), root.Actor(), u.ID), domain.ErrNotFound)
	wantErr(t, "Delete(zero)", e.svc.Delete(e.ctx(), root.Actor(), domain.UserID{}), domain.ErrInvalidID)
}

func TestPurgeExpired(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(_ *users.Deps, c *users.Config) { c.SessionIdleTimeout = time.Hour })
	alice := e.addUser("alice", domain.RoleUser)
	bob := e.addUser("bob", domain.RoleUser)
	idle := e.login("alice")
	newToken := func(u users.User, ttl time.Duration) users.APIToken {
		t.Helper()
		tok, _, err := e.svc.CreateAPIToken(e.ctx(), u.Actor(), "script", ttl)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	expiring := newToken(alice, time.Hour)
	lasting := newToken(alice, 2*time.Hour)
	forever := newToken(alice, 0)
	bobsToken := newToken(bob, 0)
	e.clock.Advance(30 * time.Minute)
	live := e.login("alice")
	bobs := e.login("bob")
	// Disable bob behind the service's back, as if the revocation failed.
	cur := e.get(bob.ID)
	cur.DisabledAt = e.clock.Now()
	if _, err := e.store.UpdateUser(e.ctx(), cur); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(31 * time.Minute) // idle is idle for 61 minutes

	n, err := e.svc.PurgeExpired(e.ctx())
	if err != nil || n != 4 {
		t.Errorf("PurgeExpired = (%d, %v), want 4 (2 sessions, 2 api tokens)", n, err)
	}
	for name, tok := range map[string]users.APIToken{"expired": expiring, "disabled user's": bobsToken} {
		if _, err := e.store.GetAPITokenByHash(e.ctx(), tok.Hash); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the %s api token was kept: %v", name, err)
		}
	}
	for name, tok := range map[string]users.APIToken{"unexpired": lasting, "non-expiring": forever} {
		if _, err := e.store.GetAPITokenByHash(e.ctx(), tok.Hash); err != nil {
			t.Errorf("the %s api token was purged: %v", name, err)
		}
	}
	for name, tok := range map[string]string{"idle": idle, "disabled user's": bobs} {
		if _, err := e.store.GetSession(e.ctx(), users.HashToken(tok)); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the %s session was kept: %v", name, err)
		}
	}
	if !e.sessionValid(live) {
		t.Error("a live session was purged")
	}
}

func TestActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := e.addUser("root", domain.RoleAdmin)
	alice := e.addUser("alice", domain.RoleUser)
	if ok, err := e.svc.Active(e.ctx(), alice.ID); err != nil || !ok {
		t.Errorf("Active(alice) = (%v, %v), want true", ok, err)
	}
	if _, err := e.svc.Update(e.ctx(), root.Actor(), alice.ID, users.UpdateParams{Version: 1, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.svc.Active(e.ctx(), alice.ID); err != nil || ok {
		t.Errorf("Active(disabled alice) = (%v, %v), want false", ok, err)
	}
	_, err := e.svc.Active(e.ctx(), domain.NewUserID())
	wantErr(t, "Active(unknown)", err, domain.ErrNotFound)
	_, err = e.svc.Active(e.ctx(), domain.UserID{})
	wantErr(t, "Active(zero)", err, domain.ErrInvalidID)
}
