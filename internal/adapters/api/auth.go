package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// MinAdminTokenLen is the shortest admin token NewAdminTokenAuth accepts.
const MinAdminTokenLen = 32

// Authenticator decides whether a call to a protected service may proceed.
// It is the seam Milestone 3 uses to replace the provisional admin token
// with users and sessions: a session authenticator would read the session
// cookie or token from header, look the user up and return a context
// carrying the identity.
type Authenticator interface {
	// Authenticate checks the credentials in header, the request headers of
	// a call to spec. On success it returns the context for the handler
	// (possibly carrying the caller's identity). A failure must wrap
	// domain.ErrUnauthenticated (it is reported as Unauthenticated); any
	// other error is reported as Internal.
	Authenticate(ctx context.Context, header http.Header, spec connect.Spec) (context.Context, error)
}

// AdminTokenAuth is the provisional Authenticator of Milestone 2: one shared
// secret sent as "Authorization: Bearer <token>". It keeps only a hash of
// the token.
type AdminTokenAuth struct {
	sum [sha256.Size]byte
}

var _ Authenticator = (*AdminTokenAuth)(nil)

// NewAdminTokenAuth returns an authenticator accepting token. The token must
// be at least MinAdminTokenLen characters. Errors never contain the token.
// Callers pass no authenticator at all (not an empty token) when the admin
// token is not configured; see Deps.Auth.
func NewAdminTokenAuth(token redact.Secret) (*AdminTokenAuth, error) {
	if token.IsZero() {
		return nil, errors.New("admin token is empty")
	}
	if len(token.Reveal()) < MinAdminTokenLen {
		return nil, fmt.Errorf("admin token is shorter than %d characters", MinAdminTokenLen)
	}
	return &AdminTokenAuth{sum: sha256.Sum256([]byte(token.Reveal()))}, nil
}

// Authenticate implements Authenticator. The comparison is constant time
// (over SHA-256 digests, so the token's length does not leak either).
func (a *AdminTokenAuth) Authenticate(ctx context.Context, header http.Header, _ connect.Spec) (context.Context, error) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ctx, fmt.Errorf("%w: missing bearer token", domain.ErrUnauthenticated)
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	if subtle.ConstantTimeCompare(got[:], a.sum[:]) != 1 {
		return ctx, fmt.Errorf("%w: wrong bearer token", domain.ErrUnauthenticated)
	}
	return ctx, nil
}

// authInterceptor applies an Authenticator to every call (unary and
// streaming) of the services it is attached to.
type authInterceptor struct {
	auth Authenticator
	log  *slog.Logger
}

var _ connect.Interceptor = (*authInterceptor)(nil)

func (i *authInterceptor) check(ctx context.Context, header http.Header, spec connect.Spec, peer string) (context.Context, error) {
	ctx, err := i.auth.Authenticate(ctx, header, spec)
	switch {
	case err == nil:
		return ctx, nil
	case errors.Is(err, domain.ErrUnauthenticated):
		// The reason is logged, never returned: callers learn nothing about
		// what was wrong.
		i.log.WarnContext(ctx, "api authentication failed", "procedure", spec.Procedure, "peer", peer, "reason", err.Error())
		cerr := connect.NewError(connect.CodeUnauthenticated, errors.New("missing or invalid credentials"))
		cerr.Meta().Set("WWW-Authenticate", `Bearer realm="syncr"`)
		return ctx, cerr
	default:
		i.log.ErrorContext(ctx, "api authentication errored", "procedure", spec.Procedure, "error", err.Error())
		return ctx, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}

// WrapUnary implements connect.Interceptor.
func (i *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := i.check(ctx, req.Header(), req.Spec(), req.Peer().Addr)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor; clients are not
// affected.
func (i *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.check(ctx, conn.RequestHeader(), conn.Spec(), conn.Peer().Addr)
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
