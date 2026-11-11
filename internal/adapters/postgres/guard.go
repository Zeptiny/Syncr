package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// controllerLockID is the key of the session-level advisory lock that a
// controller holds for its lifetime (AcquireControllerLock); it differs
// from migrationLockID, so migrating never waits for a running controller.
// "SYNCRC" in ASCII.
const controllerLockID int64 = 0x53594e435243

// OtherControllerMessage is what AcquireControllerLock logs (at WARN) when
// it finds the lock taken, and again every ControllerLockOptions.WaitLogEvery
// while it waits.
const OtherControllerMessage = "another controller is running against this database"

// Defaults of ControllerLockOptions.
const (
	DefaultLockWaitLogEvery = 30 * time.Second
	DefaultLockCheckEvery   = 10 * time.Second
	DefaultLockRetryEvery   = 5 * time.Second
	DefaultLockCheckTimeout = 10 * time.Second
)

// ControllerLockOptions tune AcquireControllerLock. Zero fields take the
// defaults.
type ControllerLockOptions struct {
	// Logger receives the lock's messages; nil discards them.
	Logger *slog.Logger
	// Clock tells the time, for how long the controller waited; nil is the
	// system clock.
	Clock interface{ Now() time.Time }
	// After returns a channel that receives once d has passed; nil is
	// time.After. Every wait of the lock goes through it (the messages
	// while waiting, the checks of the held lock, the retries), so tests
	// drive them without sleeping.
	After func(d time.Duration) <-chan time.Time
	// WaitLogEvery is how often OtherControllerMessage is repeated while
	// waiting (DefaultLockWaitLogEvery).
	WaitLogEvery time.Duration
	// CheckEvery is how often the connection holding the lock is checked
	// (DefaultLockCheckEvery), RetryEvery how long to wait before
	// connecting again after a failure (DefaultLockRetryEvery), and
	// CheckTimeout bounds one check or reconnection (DefaultLockCheckTimeout).
	CheckEvery   time.Duration
	RetryEvery   time.Duration
	CheckTimeout time.Duration
}

func (o ControllerLockOptions) withDefaults() ControllerLockOptions {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.After == nil {
		o.After = time.After
	}
	o.WaitLogEvery = cmp.Or(o.WaitLogEvery, DefaultLockWaitLogEvery)
	o.CheckEvery = cmp.Or(o.CheckEvery, DefaultLockCheckEvery)
	o.RetryEvery = cmp.Or(o.RetryEvery, DefaultLockRetryEvery)
	o.CheckTimeout = cmp.Or(o.CheckTimeout, DefaultLockCheckTimeout)
	return o
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// ControllerLock is the single-controller guard
// (docs/adr/0015-single-controller-guard.md): a session-level Postgres
// advisory lock on a dedicated connection, held from AcquireControllerLock
// until Release. While it is held, a goroutine checks the connection every
// CheckEvery. If the connection is lost, it connects again and tries to
// take the lock once per successful connection: if it gets it, nothing
// changes; if another controller holds it (it took over while this one had
// no connection), Lost is closed and the controller must stop. While the
// database cannot be reached it keeps trying, and the controller keeps
// running: a database restart alone never stops it.
type ControllerLock struct {
	dial lockDialer
	opts ControllerLockOptions
	log  *slog.Logger

	// sess and self are owned by the watch goroutine once it runs, and by
	// Release after it ended.
	sess lockSession
	self lockHolder

	lost    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	release sync.Once
}

// AcquireControllerLock takes the controller lock of the store's database
// on a connection of its own, waiting while another controller holds it:
// it logs OtherControllerMessage at once and every WaitLogEvery, and Postgres
// wakes it as soon as the lock is free. Connection failures are retried
// every RetryEvery. It returns only with the lock held, or with ctx's error
// when ctx ends first (nothing is held then).
//
// Call it before Migrate and before anything is served, and call Release
// after the store was closed (see the ADR). The in-memory store needs no
// guard.
func (s *Store) AcquireControllerLock(ctx context.Context, opts ControllerLockOptions) (*ControllerLock, error) {
	return acquireControllerLock(ctx, pgLockDialer(s.connConfig), opts)
}

// Lost is closed when the lock was lost to another controller: the
// connection holding it broke and, once connected again, another session
// held it. It is never closed by Release.
func (l *ControllerLock) Lost() <-chan struct{} { return l.lost }

// Release stops checking the lock, unlocks it and closes its connection;
// another controller waiting for it gets it then. It is idempotent and
// safe to call after Lost.
func (l *ControllerLock) Release() {
	l.release.Do(func() {
		close(l.stop)
		l.cancel()
		<-l.done
		if l.sess == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), l.opts.CheckTimeout)
		defer cancel()
		// Closing the session releases the lock too; unlocking first makes
		// it immediate even if the close is slow.
		if err := l.sess.unlock(ctx); err != nil {
			l.log.Warn("controller lock: unlocking failed; closing its connection releases it", "error", err.Error())
		}
		l.sess.close()
		l.sess = nil
		l.log.Info("controller lock released")
	})
}

// acquireControllerLock is AcquireControllerLock over dial.
func acquireControllerLock(ctx context.Context, dial lockDialer, opts ControllerLockOptions) (*ControllerLock, error) {
	opts = opts.withDefaults()
	l := &ControllerLock{
		dial: dial, opts: opts, log: opts.Logger,
		lost: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}),
	}
	start := opts.Clock.Now()
	waited := false
	for {
		sess, err := l.connect(ctx)
		if err == nil {
			var ok bool
			if ok, err = sess.tryLock(ctx); err == nil && !ok {
				waited = true
				err = l.wait(ctx, sess, start)
				ok = err == nil
			}
			if ok {
				return l.held(sess, waited, start), nil
			}
			sess.close()
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("postgres: take the controller lock: %w", context.Cause(ctx))
		}
		l.log.Warn("controller lock: cannot reach the database; retrying", "error", err.Error(), "retry_in", l.opts.RetryEvery.String())
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("postgres: take the controller lock: %w", context.Cause(ctx))
		case <-l.opts.After(l.opts.RetryEvery):
		}
	}
}

// connect opens a lock session within CheckTimeout.
func (l *ControllerLock) connect(ctx context.Context) (lockSession, error) {
	cctx, cancel := context.WithTimeout(ctx, l.opts.CheckTimeout)
	defer cancel()
	return l.dial(cctx)
}

// wait blocks in Postgres until sess gets the lock, logging
// OtherControllerMessage now and every WaitLogEvery. It returns nil once
// the lock is held, or the error that ended the wait (ctx's, or a broken
// connection).
func (l *ControllerLock) wait(ctx context.Context, sess lockSession, start time.Time) error {
	attrs := []any{}
	hctx, cancel := context.WithTimeout(ctx, l.opts.CheckTimeout)
	if h, ok, err := sess.holder(hctx); err == nil && ok {
		attrs = append(attrs, "holder_pid", h.PID, "holder_address", h.Addr, "holder_since", h.Started.UTC().Format(time.RFC3339))
	}
	cancel()
	l.log.Warn(OtherControllerMessage, append(attrs,
		"action", "waiting; nothing is served until it stops (one controller runs per database)")...)

	wctx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	granted := make(chan error, 1)
	go func() { granted <- sess.lock(wctx) }()
	for {
		select {
		case err := <-granted:
			return err
		case <-l.opts.After(l.opts.WaitLogEvery):
			l.log.Warn(OtherControllerMessage, append(attrs, "waited", l.since(start))...)
		}
	}
}

// held finishes the acquisition: it records the session and starts
// watching it.
func (l *ControllerLock) held(sess lockSession, waited bool, start time.Time) *ControllerLock {
	l.sess = sess
	l.ctx, l.cancel = context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(l.ctx, l.opts.CheckTimeout)
	l.self, _ = sess.self(ctx)
	cancel()
	if waited {
		l.log.Info("controller lock taken: the other controller stopped", "waited", l.since(start))
	} else {
		l.log.Info("controller lock taken")
	}
	go l.watch()
	return l
}

// since is the time since start, in whole seconds, for messages.
func (l *ControllerLock) since(start time.Time) string {
	return l.opts.Clock.Now().Sub(start).Round(time.Second).String()
}

// watch checks the held lock's connection every CheckEvery until Release,
// and takes the lock again when the connection is lost.
func (l *ControllerLock) watch() {
	defer close(l.done)
	for {
		select {
		case <-l.stop:
			return
		case <-l.opts.After(l.opts.CheckEvery):
		}
		ctx, cancel := context.WithTimeout(l.ctx, l.opts.CheckTimeout)
		err := l.sess.ping(ctx)
		cancel()
		if err == nil {
			continue
		}
		if l.stopping() {
			return
		}
		l.log.Warn("controller lock: the connection holding it was lost; taking it again", "error", err.Error())
		l.sess.close()
		l.sess = nil
		if !l.retake() {
			return
		}
	}
}

func (l *ControllerLock) stopping() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

// retake connects again and tries to take the lock, once per connection
// that works. It returns true with the lock held again, false when Release
// was called or the lock is lost (Lost is closed then).
func (l *ControllerLock) retake() bool {
	for {
		sess, err := l.connect(l.ctx)
		if err == nil {
			var held bool
			if held, err = l.tryRetake(sess); err == nil && held {
				return true
			}
			if err == nil {
				// Another controller has it.
				sess.close()
				l.log.Error("controller lock lost: " + OtherControllerMessage +
					" (it took over while this controller had no database connection); shutting down")
				close(l.lost)
				return false
			}
			sess.close()
		}
		if l.stopping() {
			return false
		}
		l.log.Warn("controller lock: cannot take it again yet; retrying", "error", err.Error(), "retry_in", l.opts.RetryEvery.String())
		select {
		case <-l.stop:
			return false
		case <-l.opts.After(l.opts.RetryEvery):
		}
	}
}

// errOwnSession reports that the lock is still held by this controller's
// previous session, which the server has not ended yet: it was terminated,
// and the next attempt takes the lock.
var errOwnSession = errors.New("the previous connection still holds the lock on the server; it was ended")

// tryRetake tries to take the lock on sess: true when it is held again,
// false when another controller holds it. When the holder is this
// controller's own previous session (the server has not noticed it is
// gone), it ends that session and returns errOwnSession, so the caller
// retries.
func (l *ControllerLock) tryRetake(sess lockSession) (bool, error) {
	ctx, cancel := context.WithTimeout(l.ctx, l.opts.CheckTimeout)
	defer cancel()
	ok, err := sess.tryLock(ctx)
	if err != nil {
		return false, err
	}
	if ok {
		l.sess = sess
		l.self, _ = sess.self(ctx)
		l.log.Info("controller lock taken again")
		return true, nil
	}
	h, found, err := sess.holder(ctx)
	switch {
	case err != nil:
		return false, err
	case !found:
		return false, errors.New("the lock was released meanwhile")
	case !l.self.zero() && h.same(l.self):
		if err := sess.terminate(ctx, h.PID); err != nil {
			return false, fmt.Errorf("end the previous connection: %w", err)
		}
		return false, errOwnSession
	}
	return false, nil
}

// lockHolder identifies a database session: a backend PID alone may be
// reused, the PID with the session's start time is not.
type lockHolder struct {
	PID     int32
	Started time.Time
	// Addr is the session's client address ("" for a Unix socket).
	Addr string
}

func (h lockHolder) zero() bool { return h.PID == 0 }

func (h lockHolder) same(o lockHolder) bool { return h.PID == o.PID && h.Started.Equal(o.Started) }

// lockSession is one dedicated database session for the lock.
type lockSession interface {
	// tryLock takes the lock if it is free (pg_try_advisory_lock).
	tryLock(ctx context.Context) (bool, error)
	// lock waits for the lock (pg_advisory_lock) until ctx ends.
	lock(ctx context.Context) error
	// unlock releases it (pg_advisory_unlock).
	unlock(ctx context.Context) error
	// ping checks the session.
	ping(ctx context.Context) error
	// self identifies this session; holder the session holding the lock
	// (found false if none does).
	self(ctx context.Context) (lockHolder, error)
	holder(ctx context.Context) (lockHolder, bool, error)
	// terminate ends the session of backend pid (pg_terminate_backend).
	terminate(ctx context.Context, pid int32) error
	// close closes the session, which releases the lock it holds.
	close()
}

// lockDialer opens a lock session.
type lockDialer func(ctx context.Context) (lockSession, error)

// pgLockDialer opens lock sessions with cfg, without the statement and
// lock timeouts a database URL may set (a wait for the lock is meant to
// last).
func pgLockDialer(cfg *pgx.ConnConfig) lockDialer {
	return func(ctx context.Context) (lockSession, error) {
		conn, err := pgx.ConnectConfig(ctx, cfg.Copy())
		if err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		if _, err := conn.Exec(ctx, "SET statement_timeout = 0; SET lock_timeout = 0"); err != nil {
			closeConn(conn)
			return nil, fmt.Errorf("set up the session: %w", err)
		}
		return &pgLockSession{conn: conn}, nil
	}
}

// pgLockSession is a lockSession on one pgx connection.
type pgLockSession struct{ conn *pgx.Conn }

// The lock's key as pg_locks shows a bigint advisory key: its high and low
// 32 bits in classid and objid, objsubid 1.
const (
	lockClassID = controllerLockID >> 32
	lockObjID   = controllerLockID & 0xffffffff
)

func (p *pgLockSession) tryLock(ctx context.Context) (bool, error) {
	var ok bool
	err := p.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", controllerLockID).Scan(&ok)
	return ok, err
}

func (p *pgLockSession) lock(ctx context.Context) error {
	_, err := p.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", controllerLockID)
	return err
}

func (p *pgLockSession) unlock(ctx context.Context) error {
	var ok bool
	if err := p.conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", controllerLockID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("the session did not hold the lock")
	}
	return nil
}

func (p *pgLockSession) ping(ctx context.Context) error { return p.conn.Ping(ctx) }

func (p *pgLockSession) self(ctx context.Context) (lockHolder, error) {
	var h lockHolder
	err := p.conn.QueryRow(ctx, `SELECT pid, backend_start, coalesce(host(client_addr), '')
		FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&h.PID, &h.Started, &h.Addr)
	return h, err
}

func (p *pgLockSession) holder(ctx context.Context) (lockHolder, bool, error) {
	var h lockHolder
	err := p.conn.QueryRow(ctx, `SELECT a.pid, a.backend_start, coalesce(host(a.client_addr), '')
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND l.classid::bigint = $1 AND l.objid::bigint = $2`, lockClassID, lockObjID).Scan(&h.PID, &h.Started, &h.Addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockHolder{}, false, nil
	}
	return h, err == nil, err
}

func (p *pgLockSession) terminate(ctx context.Context, pid int32) error {
	_, err := p.conn.Exec(ctx, "SELECT pg_terminate_backend($1)", pid)
	return err
}

func (p *pgLockSession) close() { closeConn(p.conn) }

// closeConn closes conn, waiting at most a few seconds for the server.
func closeConn(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
