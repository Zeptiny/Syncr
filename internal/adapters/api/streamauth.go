package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
)

// defaultStreamReauth is how often the guard re-authenticates a streaming
// call (Deps.StreamReauthInterval).
const defaultStreamReauth = 30 * time.Second

// A streaming call authenticates once when it starts, but may stay open
// for an hour (WatchJob). So that signing out, revoking the session or API
// token, disabling the user or taking away the admin role also ends the
// streams opened with those credentials, the guard repeats its check (the
// same Authenticator call and policy check) every reauth interval while the
// stream runs, and then the access checks the handler registered with
// onReauth, given the refreshed Caller. The first failure cancels the
// handler's context, and the stream ends with that failure's error
// (Unauthenticated, PermissionDenied, or Internal if the check itself
// errored: the guard fails closed).

// reauthKey is the context key of a streaming call's *streamReauth.
type reauthKey struct{}

// streamReauth holds the access checks a streaming handler registered.
type streamReauth struct {
	mu     sync.Mutex
	checks []func(ctx context.Context, c Caller) error
}

// onReauth registers check to run with the refreshed caller each time the
// guard re-authenticates the streaming call of ctx. A non-nil result ends
// the stream with that error, which should be a *connect.Error (anything
// else is reported as Internal). It is a no-op outside a streaming call.
func onReauth(ctx context.Context, check func(ctx context.Context, c Caller) error) {
	if r, ok := ctx.Value(reauthKey{}).(*streamReauth); ok {
		r.mu.Lock()
		r.checks = append(r.checks, check)
		r.mu.Unlock()
	}
}

func (r *streamReauth) run(ctx context.Context, c Caller) error {
	r.mu.Lock()
	checks := append([]func(context.Context, Caller) error(nil), r.checks...)
	r.mu.Unlock()
	for _, check := range checks {
		if err := check(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// revokedError is the cancel cause of a stream whose caller no longer
// passes the guard; err is the error the stream ends with.
type revokedError struct{ err error }

func (e revokedError) Error() string { return "stream access revoked: " + e.err.Error() }

// serveReauthenticated runs next for an authenticated streaming call (actx
// is the context check returned for ctx), re-checking the caller every
// g.reauth until next returns.
func (g *guard) serveReauthenticated(ctx, actx context.Context, conn connect.StreamingHandlerConn, next connect.StreamingHandlerFunc) error {
	r := &streamReauth{}
	sctx, cancel := context.WithCancelCause(context.WithValue(actx, reauthKey{}, r))
	defer cancel(nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(g.reauth)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-sctx.Done():
				return
			case <-tick.C:
			}
			if err := g.recheck(ctx, conn, r); err != nil {
				if sctx.Err() == nil { // not just a client that went away
					cancel(revokedError{err: err})
				}
				return
			}
		}
	}()

	err := next(sctx, conn)
	close(stop)
	<-done
	var revoked revokedError
	if err != nil && errors.As(context.Cause(sctx), &revoked) {
		return revoked.err
	}
	return err
}

// recheck repeats the guard's check of a streaming call and the handler's
// access checks.
func (g *guard) recheck(ctx context.Context, conn connect.StreamingHandlerConn, r *streamReauth) error {
	actx, err := g.check(ctx, conn.RequestHeader(), conn.Spec(), conn.Peer().Addr)
	if err != nil {
		return err
	}
	c, _ := CallerFrom(actx) // check guarantees it
	err = r.run(actx, c)
	var cerr *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &cerr):
		g.log.InfoContext(ctx, "api stream ended: the caller lost access", "procedure", conn.Spec().Procedure,
			"user_id", c.Actor.UserID.String(), "code", cerr.Code().String())
		return err
	default:
		g.log.ErrorContext(ctx, "api stream access check errored", "procedure", conn.Spec().Procedure, "error", err.Error())
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
