package postgres

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// watchdog bounds every wait of these tests for an event that must come;
// it is never what synchronizes them.
const watchdog = 10 * time.Second

// fakeLockServer is a database with one advisory lock: sessions take it,
// wait for it, and lose it when they end.
type fakeLockServer struct {
	mu      sync.Mutex
	nextPID int32
	holder  *fakeLockSession
	// changed is closed and replaced on every change of the holder.
	changed chan struct{}
	// dialErrs fail the next dials, in order.
	dialErrs []error
	sessions []*fakeLockSession
}

func newFakeLockServer() *fakeLockServer {
	return &fakeLockServer{nextPID: 100, changed: make(chan struct{})}
}

func (s *fakeLockServer) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *fakeLockServer) dial(context.Context) (lockSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dialErrs) > 0 {
		err := s.dialErrs[0]
		s.dialErrs = s.dialErrs[1:]
		return nil, err
	}
	s.nextPID++
	sess := &fakeLockSession{srv: s, pid: s.nextPID, started: time.Unix(int64(s.nextPID), 0), alive: true}
	s.sessions = append(s.sessions, sess)
	return sess, nil
}

// kill ends the holder's session on the server (pg_terminate_backend): its
// lock goes to a waiter, its next call fails.
func (s *fakeLockServer) kill(t *testing.T) *fakeLockSession {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.holder
	if h == nil {
		t.Fatal("nobody holds the lock")
	}
	h.endLocked()
	return h
}

// cut makes the holder unreachable from its client while the server keeps
// the session and the lock (a network failure the server has not noticed).
func (s *fakeLockServer) cut(t *testing.T) *fakeLockSession {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder == nil {
		t.Fatal("nobody holds the lock")
	}
	s.holder.unreachable = true
	return s.holder
}

func (s *fakeLockServer) holderPID() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder == nil {
		return 0
	}
	return s.holder.pid
}

type fakeLockSession struct {
	srv         *fakeLockServer
	pid         int32
	started     time.Time
	alive       bool
	unreachable bool
}

var errSessionGone = errors.New("terminating connection due to administrator command")

// endLocked ends the session on the server.
func (f *fakeLockSession) endLocked() {
	f.alive = false
	if f.srv.holder == f {
		f.srv.holder = nil
		f.srv.notifyLocked()
	}
}

func (f *fakeLockSession) usableLocked() error {
	if !f.alive || f.unreachable {
		return errSessionGone
	}
	return nil
}

func (f *fakeLockSession) tryLock(context.Context) (bool, error) {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	if err := f.usableLocked(); err != nil {
		return false, err
	}
	if f.srv.holder == nil {
		f.srv.holder = f
		f.srv.notifyLocked()
	}
	return f.srv.holder == f, nil
}

func (f *fakeLockSession) lock(ctx context.Context) error {
	for {
		f.srv.mu.Lock()
		if err := f.usableLocked(); err != nil {
			f.srv.mu.Unlock()
			return err
		}
		if f.srv.holder == nil || f.srv.holder == f {
			f.srv.holder = f
			f.srv.notifyLocked()
			f.srv.mu.Unlock()
			return nil
		}
		changed := f.srv.changed
		f.srv.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (f *fakeLockSession) unlock(context.Context) error {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	if err := f.usableLocked(); err != nil {
		return err
	}
	if f.srv.holder != f {
		return errors.New("not held")
	}
	f.srv.holder = nil
	f.srv.notifyLocked()
	return nil
}

func (f *fakeLockSession) ping(context.Context) error {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	return f.usableLocked()
}

func (f *fakeLockSession) self(context.Context) (lockHolder, error) {
	return lockHolder{PID: f.pid, Started: f.started}, nil
}

func (f *fakeLockSession) holder(context.Context) (lockHolder, bool, error) {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	if err := f.usableLocked(); err != nil {
		return lockHolder{}, false, err
	}
	if h := f.srv.holder; h != nil {
		return lockHolder{PID: h.pid, Started: h.started, Addr: "10.0.0.9"}, true, nil
	}
	return lockHolder{}, false, nil
}

func (f *fakeLockSession) terminate(_ context.Context, pid int32) error {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	if err := f.usableLocked(); err != nil {
		return err
	}
	for _, s := range f.srv.sessions {
		if s.pid == pid {
			s.endLocked()
		}
	}
	return nil
}

// close ends the session, unless it is unreachable: the server then keeps
// it (and its lock) until it is terminated.
func (f *fakeLockSession) close() {
	f.srv.mu.Lock()
	defer f.srv.mu.Unlock()
	if !f.unreachable {
		f.endLocked()
	}
}

// fakeTimers is ControllerLockOptions.After: every wait is a request the
// test fires.
type fakeTimers struct{ reqs chan timerReq }

type timerReq struct {
	d time.Duration
	c chan time.Time
}

func newFakeTimers() *fakeTimers { return &fakeTimers{reqs: make(chan timerReq, 64)} }

func (f *fakeTimers) After(d time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.reqs <- timerReq{d, c}
	return c
}

// next waits until the lock waits again, which must be for d, and returns
// that wait without ending it.
func (f *fakeTimers) next(t *testing.T, d time.Duration) timerReq {
	t.Helper()
	select {
	case r := <-f.reqs:
		if r.d != d {
			t.Fatalf("the lock waits %v, want %v", r.d, d)
		}
		return r
	case <-time.After(watchdog):
		t.Fatalf("the lock never waited %v", d)
	}
	return timerReq{}
}

// fire waits for the next wait of the lock, which must be d long, and ends
// it.
func (f *fakeTimers) fire(t *testing.T, d time.Duration) {
	t.Helper()
	f.next(t, d).c <- time.Time{}
}

// logRecords is a slog handler that passes every message on.
type logRecords struct {
	msgs chan slog.Record
}

func (h logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (h logRecords) Handle(_ context.Context, r slog.Record) error {
	h.msgs <- r
	return nil
}
func (h logRecords) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h logRecords) WithGroup(string) slog.Handler      { return h }

// expect waits for the next message, which must start with want.
func (h logRecords) expect(t *testing.T, want string) slog.Record {
	t.Helper()
	select {
	case r := <-h.msgs:
		if !strings.HasPrefix(r.Message, want) {
			t.Fatalf("logged %q (%v), want %q", r.Message, r.Level, want)
		}
		return r
	case <-time.After(watchdog):
		t.Fatalf("%q was never logged", want)
	}
	return slog.Record{}
}

// quiet fails if anything is waiting to be logged.
func (h logRecords) quiet(t *testing.T) {
	t.Helper()
	select {
	case r := <-h.msgs:
		t.Fatalf("unexpected log %q", r.Message)
	default:
	}
}

// guardEnv is one controller's lock on a fake server.
type guardEnv struct {
	timers *fakeTimers
	logs   logRecords
	opts   ControllerLockOptions
}

func newGuardEnv() *guardEnv {
	g := &guardEnv{timers: newFakeTimers(), logs: logRecords{msgs: make(chan slog.Record, 64)}}
	g.opts = ControllerLockOptions{Logger: slog.New(g.logs), After: g.timers.After}
	return g
}

type acquired struct {
	lock *ControllerLock
	err  error
}

func (g *guardEnv) acquire(ctx context.Context, srv *fakeLockServer) <-chan acquired {
	out := make(chan acquired, 1)
	go func() {
		l, err := acquireControllerLock(ctx, srv.dial, g.opts)
		out <- acquired{l, err}
	}()
	return out
}

func got(t *testing.T, c <-chan acquired) *ControllerLock {
	t.Helper()
	select {
	case a := <-c:
		if a.err != nil {
			t.Fatalf("acquire: %v", a.err)
		}
		return a.lock
	case <-time.After(watchdog):
		t.Fatal("the lock was never acquired")
	}
	return nil
}

func pending(t *testing.T, c <-chan acquired) {
	t.Helper()
	select {
	case a := <-c:
		t.Fatalf("acquired while another controller holds the lock: %+v", a)
	default:
	}
}

func isLost(l *ControllerLock) bool {
	select {
	case <-l.Lost():
		return true
	default:
		return false
	}
}

// TestControllerLockWaitsForTheHolder: a second controller waits, logs
// that another controller runs at once and every 30 seconds, and takes the
// lock as soon as the first releases it.
func TestControllerLockWaitsForTheHolder(t *testing.T) {
	t.Parallel()
	srv := newFakeLockServer()
	ctx := context.Background()
	a, b := newGuardEnv(), newGuardEnv()

	first := got(t, a.acquire(ctx, srv))
	a.logs.expect(t, "controller lock taken")
	firstPID := srv.holderPID()

	second := b.acquire(ctx, srv)
	r := b.logs.expect(t, OtherControllerMessage)
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", r.Level)
	}
	holder := map[string]string{}
	r.Attrs(func(at slog.Attr) bool { holder[at.Key] = at.Value.String(); return true })
	if holder["holder_address"] != "10.0.0.9" || holder["holder_pid"] == "" {
		t.Errorf("the message does not name the holder: %v", holder)
	}
	for range 3 {
		b.timers.fire(t, DefaultLockWaitLogEvery)
		b.logs.expect(t, OtherControllerMessage)
		pending(t, second)
	}
	if srv.holderPID() != firstPID {
		t.Fatal("the waiting controller took the lock")
	}

	first.Release()
	a.logs.expect(t, "controller lock released")
	l := got(t, second)
	b.logs.expect(t, "controller lock taken: the other controller stopped")
	if srv.holderPID() == firstPID || srv.holderPID() == 0 {
		t.Errorf("holder = %d after the release", srv.holderPID())
	}
	l.Release()
	l.Release() // idempotent
	if srv.holderPID() != 0 {
		t.Error("released, but still held")
	}
	if isLost(l) || isLost(first) {
		t.Error("a released lock is lost")
	}
}

// TestControllerLockTakesItAgain: when the connection holding the lock is
// lost and nobody else holds it, the next check connects again, takes it
// and carries on; connection failures are retried.
func TestControllerLockTakesItAgain(t *testing.T) {
	t.Parallel()
	srv := newFakeLockServer()
	g := newGuardEnv()
	l := got(t, g.acquire(context.Background(), srv))
	defer l.Release()
	g.logs.expect(t, "controller lock taken")

	g.timers.fire(t, DefaultLockCheckEvery) // a healthy check
	check := g.timers.next(t, DefaultLockCheckEvery)
	old := srv.kill(t)
	srv.mu.Lock()
	srv.dialErrs = []error{errors.New("connection refused")} // the database restarts
	srv.mu.Unlock()
	check.c <- time.Time{}
	g.logs.expect(t, "controller lock: the connection holding it was lost")
	g.logs.expect(t, "controller lock: cannot take it again yet")
	g.timers.fire(t, DefaultLockRetryEvery)
	g.logs.expect(t, "controller lock taken again")
	if pid := srv.holderPID(); pid == 0 || pid == old.pid {
		t.Errorf("holder = %d, want a new session", pid)
	}
	if isLost(l) {
		t.Error("the lock is lost although nobody else took it")
	}
	g.timers.fire(t, DefaultLockCheckEvery) // it watches the new session
	g.logs.quiet(t)
}

// TestControllerLockLostToAnother: when another controller took the lock
// while this one's connection was gone, Lost is closed.
func TestControllerLockLostToAnother(t *testing.T) {
	t.Parallel()
	srv := newFakeLockServer()
	ctx := context.Background()
	a, b := newGuardEnv(), newGuardEnv()
	first := got(t, a.acquire(ctx, srv))
	defer first.Release()
	a.logs.expect(t, "controller lock taken")
	second := b.acquire(ctx, srv)
	b.logs.expect(t, OtherControllerMessage)

	srv.kill(t) // the first's session ends: the waiting one gets the lock
	other := got(t, second)
	defer other.Release()
	a.timers.fire(t, DefaultLockCheckEvery)
	a.logs.expect(t, "controller lock: the connection holding it was lost")
	r := a.logs.expect(t, "controller lock lost")
	if r.Level != slog.LevelError || !strings.Contains(r.Message, "shutting down") {
		t.Errorf("logged %v %q", r.Level, r.Message)
	}
	select {
	case <-first.Lost():
	case <-time.After(watchdog):
		t.Fatal("Lost was not closed")
	}
	if isLost(other) {
		t.Error("the new holder lost the lock")
	}
}

// TestControllerLockEndsItsOwnLingeringSession: a connection that broke on
// the client's side while the server still holds the lock for it is ended,
// and the lock taken again, rather than mistaken for another controller.
func TestControllerLockEndsItsOwnLingeringSession(t *testing.T) {
	t.Parallel()
	srv := newFakeLockServer()
	g := newGuardEnv()
	l := got(t, g.acquire(context.Background(), srv))
	defer l.Release()
	g.logs.expect(t, "controller lock taken")

	// Closing the old connection cannot reach the server either: the server
	// keeps it, with the lock, until the next attempt ends it.
	old := srv.cut(t)
	g.timers.fire(t, DefaultLockCheckEvery)
	g.logs.expect(t, "controller lock: the connection holding it was lost")
	r := g.logs.expect(t, "controller lock: cannot take it again yet")
	var reason string
	r.Attrs(func(at slog.Attr) bool {
		if at.Key == "error" {
			reason = at.Value.String()
		}
		return true
	})
	if !strings.Contains(reason, "previous connection") {
		t.Errorf("retry reason %q", reason)
	}
	g.timers.fire(t, DefaultLockRetryEvery)
	g.logs.expect(t, "controller lock taken again")
	if isLost(l) || srv.holderPID() == old.pid {
		t.Error("the controller's own old session was taken for another controller")
	}
}

// TestControllerLockWaitEndsWithTheContext: a controller that stops while
// it waits returns its context's error and leaves nothing behind.
func TestControllerLockWaitEndsWithTheContext(t *testing.T) {
	t.Parallel()
	srv := newFakeLockServer()
	a, b := newGuardEnv(), newGuardEnv()
	first := got(t, a.acquire(context.Background(), srv))
	defer first.Release()
	ctx, cancel := context.WithCancel(context.Background())
	second := b.acquire(ctx, srv)
	b.logs.expect(t, OtherControllerMessage)
	cancel()
	select {
	case res := <-second:
		if !errors.Is(res.err, context.Canceled) || res.lock != nil {
			t.Fatalf("acquire = %+v", res)
		}
	case <-time.After(watchdog):
		t.Fatal("the wait did not end with its context")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, s := range srv.sessions[1:] {
		if s.alive {
			t.Errorf("session %d is still open", s.pid)
		}
	}
}

// TestControllerLockRetriesConnecting: a database that cannot be reached
// at start is retried.
func TestControllerLockRetriesConnecting(t *testing.T) {
	t.Parallel()
	srv := newFakeLockServer()
	srv.dialErrs = []error{errors.New("connection refused"), errors.New("connection refused")}
	g := newGuardEnv()
	res := g.acquire(context.Background(), srv)
	for range 2 {
		g.logs.expect(t, "controller lock: cannot reach the database")
		pending(t, res)
		g.timers.fire(t, DefaultLockRetryEvery)
	}
	l := got(t, res)
	l.Release()
	if controllerLockID == migrationLockID {
		t.Error("the controller lock and the migration lock share a key")
	}
	if lockClassID<<32|lockObjID != controllerLockID {
		t.Error("the pg_locks key does not match the lock")
	}
}
