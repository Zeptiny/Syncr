package api

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// AuthMethod is how a call authenticated.
type AuthMethod int

// Authentication methods.
const (
	// AuthNone: the call did not authenticate (public procedures).
	AuthNone AuthMethod = iota
	// AuthSession: the web UI's session cookie.
	AuthSession
	// AuthToken: a bearer token (a personal API token, or the provisional
	// admin token of Milestone 2).
	AuthToken
)

// String returns the method's name, for logs.
func (m AuthMethod) String() string {
	switch m {
	case AuthSession:
		return "session"
	case AuthToken:
		return "token"
	default:
		return "none"
	}
}

// Caller is the authenticated caller of a call. The Authenticator puts it
// into the call's context (WithCaller); the policy interceptor checks it
// against the procedure's access level, and handlers read the actor with
// ActorFrom and pass it explicitly to the use cases.
type Caller struct {
	Actor  domain.Actor
	Method AuthMethod
	// ExpiresAt is when the session or token stops working; zero if never
	// or unknown.
	ExpiresAt time.Time
	// SessionToken is the raw session token of an AuthSession call (Logout
	// ends that session, ChangePassword keeps it); empty otherwise.
	SessionToken redact.Secret
}

// callerKey is the context key of the Caller.
type callerKey struct{}

// WithCaller returns ctx carrying c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller in ctx; ok is false for unauthenticated
// (public) calls.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// ActorFrom returns the actor of the call in ctx; ok is false for
// unauthenticated (public) calls. Handlers of protected procedures always
// find one: the interceptor rejects calls that did not authenticate.
func ActorFrom(ctx context.Context) (domain.Actor, bool) {
	c, ok := CallerFrom(ctx)
	return c.Actor, ok
}
