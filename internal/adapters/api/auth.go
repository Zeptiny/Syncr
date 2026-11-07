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

	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// MinAdminTokenLen is the shortest admin token NewAdminTokenAuth accepts.
const MinAdminTokenLen = 32

// Authenticator identifies the caller of a non-public procedure (see
// policy.go). SessionAuth is the controller's (the session cookie with its
// CSRF header, or a personal API token); AdminTokenAuth is the provisional
// authenticator of Milestone 2, kept until the integration pass wires
// SessionAuth.
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

// CSRFHeader must accompany every call that authenticates with the session
// cookie, and Login and CompleteSetup, with the value "1". The web UI's
// transport adds it to every call. CORS is never enabled, so another
// origin cannot send it (a custom header needs a preflight, which fails);
// bearer-token calls need none, as they carry no ambient credential.
const CSRFHeader = "X-Syncr-CSRF"

// SessionAuth is the controller's Authenticator. A call authenticates with
// either
//
//   - "Authorization: Bearer syncr_pat_..." (a personal API token; if it is
//     wrong the call fails, whatever cookie comes with it), or
//   - the session cookie (CookieConfig.Name for the request's Secure mode;
//     the other name is ignored), together with CSRFHeader: a cookie
//     without the header is not a credential, and more than one cookie of
//     the name is refused (cookie tossing).
//
// Authorization headers that are not personal API tokens (a reverse
// proxy's basic auth, another bearer format) are ignored. It puts a Caller
// into the context: AuthToken with the token's expiry, or AuthSession with
// the session's absolute expiry and raw token (for Logout and
// ChangePassword). Every failure wraps domain.ErrUnauthenticated and says
// why for the log, never with the credential.
type SessionAuth struct {
	creds   CredentialVerifier
	cookies CookieConfig
}

var _ Authenticator = (*SessionAuth)(nil)

// NewSessionAuth returns the authenticator over creds (*users.Service). The
// cookie configuration must be the one of Deps.Cookies, so it reads the
// cookie that Login sets.
func NewSessionAuth(creds CredentialVerifier, cookies CookieConfig) (*SessionAuth, error) {
	if creds == nil {
		return nil, errors.New("session auth: no credential verifier")
	}
	if err := cookies.Validate(); err != nil {
		return nil, fmt.Errorf("session auth: %w", err)
	}
	return &SessionAuth{creds: creds, cookies: cookies}, nil
}

// Authenticate implements Authenticator.
func (a *SessionAuth) Authenticate(ctx context.Context, header http.Header, _ connect.Spec) (context.Context, error) {
	values := header.Values("Authorization")
	tokens := 0
	for _, v := range values {
		if _, ok := personalToken(v); ok {
			tokens++
		}
	}
	switch {
	case tokens > 1 || (tokens == 1 && len(values) > 1):
		return ctx, fmt.Errorf("%w: several Authorization headers", domain.ErrUnauthenticated)
	case tokens == 1:
		token, _ := personalToken(values[0])
		return a.bearer(ctx, token)
	}
	return a.session(ctx, header, len(values) > 0)
}

// personalToken returns the personal API token of an Authorization header
// value ("Bearer syncr_pat_...", the scheme in any case).
func personalToken(value string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, strings.HasPrefix(token, users.APITokenPrefix)
}

func (a *SessionAuth) bearer(ctx context.Context, token string) (context.Context, error) {
	actor, tok, err := a.creds.AuthenticateAPIToken(ctx, token)
	if err != nil {
		return ctx, err
	}
	return WithCaller(ctx, Caller{Actor: actor, Method: AuthToken, ExpiresAt: tok.ExpiresAt}), nil
}

// session authenticates with the cookie; foreignAuth tells that an
// Authorization header other than a personal API token was ignored.
func (a *SessionAuth) session(ctx context.Context, header http.Header, foreignAuth bool) (context.Context, error) {
	name := a.cookies.Name(a.cookies.SecureFor(requestTLS(ctx)))
	cookies := (&http.Request{Header: header}).CookiesNamed(name)
	switch {
	case len(cookies) == 0 && foreignAuth:
		return ctx, fmt.Errorf("%w: no credentials (the Authorization header is not a personal API token)", domain.ErrUnauthenticated)
	case len(cookies) == 0:
		return ctx, fmt.Errorf("%w: no credentials", domain.ErrUnauthenticated)
	case len(cookies) > 1:
		return ctx, fmt.Errorf("%w: %d session cookies", domain.ErrUnauthenticated, len(cookies))
	case header.Get(CSRFHeader) != "1":
		return ctx, fmt.Errorf("%w: session cookie without the %s header", domain.ErrUnauthenticated, CSRFHeader)
	}
	token := cookies[0].Value
	actor, sess, err := a.creds.AuthenticateSession(ctx, token)
	if err != nil {
		return ctx, err
	}
	return WithCaller(ctx, Caller{Actor: actor, Method: AuthSession, ExpiresAt: sess.ExpiresAt, SessionToken: redact.Secret(token)}), nil
}

// tlsKey is the context key of the request's TLS state.
type tlsKey struct{}

// withTLSState records in each request's context whether it came over TLS,
// for the session cookie's Secure mode (CookieSecureAuto): Connect
// handlers and authenticators see the headers, not the connection.
func withTLSState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tlsKey{}, r.TLS != nil)))
	})
}

// requestTLS reports whether the request of ctx came over TLS (false if
// unknown).
func requestTLS(ctx context.Context) bool {
	v, _ := ctx.Value(tlsKey{}).(bool)
	return v
}
