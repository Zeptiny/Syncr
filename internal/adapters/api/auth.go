package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// MinAdminTokenLen is the shortest admin token NewAdminTokenAuth accepts.
const MinAdminTokenLen = 32

// Authenticator identifies the caller of a non-public procedure (see
// policy.go). Milestone 3 track A provides SessionAuth (the session cookie
// with its CSRF header, or a personal API token); AdminTokenAuth is the
// provisional authenticator of Milestone 2, kept until the integration pass
// wires SessionAuth.
type Authenticator interface {
	// Authenticate checks the credentials in header, the request headers of
	// a call to spec. On success it returns the context for the handler,
	// which must carry the Caller (WithCaller); the policy interceptor then
	// checks the caller's role and method against the procedure. A failure
	// must wrap domain.ErrUnauthenticated (it is reported as
	// Unauthenticated); any other error is reported as Internal.
	Authenticate(ctx context.Context, header http.Header, spec connect.Spec) (context.Context, error)
}

// AdminTokenAuth is the provisional Authenticator of Milestone 2: one shared
// secret sent as "Authorization: Bearer <token>". It keeps only a hash of
// the token. A caller with the token acts as an admin without a user
// account (domain.Actor with a zero UserID): it may manage workers but owns
// nothing, so it cannot create remotes or run jobs.
//
// m3: the integration pass removes it in favor of personal API tokens.
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
	return WithCaller(ctx, Caller{Actor: domain.Actor{Role: domain.RoleAdmin}, Method: AuthToken}), nil
}
