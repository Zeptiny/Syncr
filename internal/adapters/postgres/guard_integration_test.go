//go:build integration

package postgres

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockHolderPID returns the backend PID holding the controller lock of s's
// database; 0 if nobody holds it.
func lockHolderPID(t *testing.T, s *Store) int32 {
	t.Helper()
	var pid int32
	err := s.pool.QueryRow(t.Context(), `SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND classid::bigint = $1 AND objid::bigint = $2`, lockClassID, lockObjID).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// terminate ends a backend and waits until it is gone.
func terminate(t *testing.T, s *Store, pid int32) {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1, 10000)", pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("terminate backend %d: %v, %v", pid, ok, err)
	}
}

func (g *guardEnv) acquireOn(ctx context.Context, s *Store) <-chan acquired {
	out := make(chan acquired, 1)
	go func() {
		l, err := s.AcquireControllerLock(ctx, g.opts)
		out <- acquired{l, err}
	}()
	return out
}

// TestControllerLockSessionKeepalives: the lock session asks the server for
// TCP keepalives, so a controller whose host crashed loses its lock after
// about a minute instead of the operating system's two hours, and has no
// statement or lock timeout.
func TestControllerLockSessionKeepalives(t *testing.T) {
	name := createDB(t, true)
	s := openStore(t, name)
	sess, err := pgLockDialer(s.connConfig)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()
	pg, ok := sess.(*pgLockSession)
	if !ok {
		t.Fatalf("the dialer returned a %T", sess)
	}
	conn := pg.conn
	for setting, want := range map[string]string{
		"tcp_keepalives_idle":     strconv.Itoa(lockKeepaliveIdle),
		"tcp_keepalives_interval": strconv.Itoa(lockKeepaliveInterval),
		"tcp_keepalives_count":    strconv.Itoa(lockKeepaliveCount),
		"tcp_user_timeout":        strconv.Itoa(lockUserTimeoutMillis),
		"statement_timeout":       "0",
		"lock_timeout":            "0",
	} {
		var got string
		if err := conn.QueryRow(t.Context(), "SELECT current_setting($1)", setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %q on the lock session, want %q (a TCP connection)", setting, got, want)
		}
	}
}

// TestControllerLockOnPostgres: two controllers on one database. The
// second waits (and says so every 30 seconds) until the first releases the
// lock; a lock connection killed on the server is taken again; one killed
// while another controller waits is lost, and the holder learns it at its
// next check. Migrations never wait for the guard.
func TestControllerLockOnPostgres(t *testing.T) {
	name := createDB(t, true)
	s1, s2, admin := openStore(t, name), openStore(t, name), openStore(t, name)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, b := newGuardEnv(), newGuardEnv()
	first := got(t, a.acquireOn(ctx, s1))
	defer first.Release()
	a.logs.expect(t, "controller lock taken")
	firstPID := lockHolderPID(t, admin)
	if firstPID == 0 {
		t.Fatal("nobody holds the lock after it was taken")
	}
	migrated := make(chan error, 1)
	go func() { migrated <- s2.Migrate(ctx) }()
	select {
	case err := <-migrated:
		if err != nil {
			t.Fatalf("Migrate while a controller runs: %v", err)
		}
	case <-time.After(watchdog):
		t.Fatal("Migrate waits for the controller lock")
	}

	second := b.acquireOn(ctx, s2)
	r := b.logs.expect(t, OtherControllerMessage)
	var holder string
	r.Attrs(func(at slog.Attr) bool {
		if at.Key == "holder_pid" {
			holder = at.Value.String()
		}
		return true
	})
	if holder != strconv.Itoa(int(firstPID)) {
		t.Errorf("holder_pid = %q, want %d", holder, firstPID)
	}
	for range 2 {
		b.timers.fire(t, DefaultLockWaitLogEvery)
		b.logs.expect(t, OtherControllerMessage)
		pending(t, second)
	}
	if lockHolderPID(t, admin) != firstPID {
		t.Fatal("the second controller took the lock")
	}

	first.Release()
	l := got(t, second)
	defer l.Release()
	b.logs.expect(t, "controller lock taken: the other controller stopped")
	b.timers.next(t, DefaultLockWaitLogEvery) // the message wait it abandoned
	secondPID := lockHolderPID(t, admin)
	if secondPID == 0 || secondPID == firstPID {
		t.Fatalf("holder = %d after the release (first %d)", secondPID, firstPID)
	}

	// The lock connection is killed: the next check takes the lock again.
	terminate(t, admin, secondPID)
	b.timers.fire(t, DefaultLockCheckEvery)
	b.logs.expect(t, "controller lock: the connection holding it was lost")
	b.logs.expect(t, "controller lock taken again")
	again := lockHolderPID(t, admin)
	if again == 0 || again == secondPID {
		t.Fatalf("holder = %d after the reconnection (killed %d)", again, secondPID)
	}
	if isLost(l) {
		t.Fatal("lost although nobody else holds the lock")
	}

	// Killed while another controller waits: the waiting one gets the lock
	// at once, and the old holder learns it at its next check.
	c := newGuardEnv()
	third := c.acquireOn(ctx, s1)
	c.logs.expect(t, OtherControllerMessage)
	terminate(t, admin, again)
	other := got(t, third)
	defer other.Release()
	b.timers.fire(t, DefaultLockCheckEvery)
	b.logs.expect(t, "controller lock: the connection holding it was lost")
	b.logs.expect(t, "controller lock lost")
	select {
	case <-l.Lost():
	case <-time.After(watchdog):
		t.Fatal("Lost was not closed")
	}
	if pid := lockHolderPID(t, admin); pid == 0 || pid == again {
		t.Errorf("holder = %d, want the third controller", pid)
	}

	other.Release()
	if pid := lockHolderPID(t, admin); pid != 0 {
		t.Errorf("holder = %d after every release", pid)
	}
}
