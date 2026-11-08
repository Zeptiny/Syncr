package api

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/password"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/ratelimit"
)

const (
	idSetupCode = "ABCDE-FGHIJ-KLMNO-PQRS2"
	idPassword  = "correct horse battery"
	idPassword2 = "another long password"
	thirtyDays  = 30 * 24 * time.Hour
)

// identityEnv is an API server on the real users service (memstore, a fast
// argon2id hasher, real rate limiters on a fake clock) behind SessionAuth.
type identityEnv struct {
	t       *testing.T
	srv     *httptest.Server
	svc     *users.Service
	store   *memstore.Store
	clock   *clock.Fake
	logs    *logBuffer
	cookies CookieConfig
}

type identityOptions struct {
	cookies CookieConfig
	tls     bool
	// loginIPBurst overrides the per-IP burst of logins and setups (default
	// 100).
	loginIPBurst int
}

func newIdentityEnv(t *testing.T, o identityOptions) *identityEnv {
	t.Helper()
	e := &identityEnv{t: t, store: memstore.New(), clock: clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)), logs: &logBuffer{}, cookies: o.cookies}
	hasher, err := password.New(password.Config{Params: password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	limiter := func(burst int) *ratelimit.Limiter {
		l, err := ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Minute, MaxKeys: 100}, e.clock)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	ipBurst := 100
	if o.loginIPBurst > 0 {
		ipBurst = o.loginIPBurst
	}
	e.svc, err = users.New(users.Deps{
		Users: e.store, Sessions: e.store, Tokens: e.store, Hasher: hasher,
		Limiters: users.Limiters{ClientIP: limiter(ipBurst), Username: limiter(100), Setup: limiter(100)},
		Clock:    e.clock, IDs: &idgen.Sequential{}, Random: crand.Reader,
	}, users.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ArmSetup(idSetupCode); err != nil {
		t.Fatal(err)
	}
	auth, err := NewSessionAuth(e.svc, o.cookies)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewUnstartedServer(NewHandler(Deps{
		Logger: newTestLogger(e.logs), Auth: auth, Identity: e.svc, Users: e.svc, Cookies: o.cookies,
	}))
	if o.tls {
		e.srv.StartTLS()
	} else {
		e.srv.Start()
	}
	t.Cleanup(e.srv.Close)
	return e
}

// creds are what a test client sends with every call.
type creds struct {
	// cookie is a "name=value" Cookie header ("" sends none).
	cookie string
	csrf   bool
	// authorization is the Authorization header ("" sends none).
	authorization string
}

func bearer(secret string) creds { return creds{authorization: "Bearer " + secret} }

func (c creds) interceptor() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if c.cookie != "" {
				req.Header().Add("Cookie", c.cookie)
			}
			if c.csrf {
				req.Header().Set(CSRFHeader, "1")
			}
			if c.authorization != "" {
				req.Header().Set("Authorization", c.authorization)
			}
			return next(ctx, req)
		}
	})
}

func (e *identityEnv) setupClient(c creds) syncrv1connect.SetupServiceClient {
	return syncrv1connect.NewSetupServiceClient(e.srv.Client(), e.srv.URL, connect.WithInterceptors(c.interceptor()))
}

func (e *identityEnv) authClient(c creds) syncrv1connect.AuthServiceClient {
	return syncrv1connect.NewAuthServiceClient(e.srv.Client(), e.srv.URL, connect.WithInterceptors(c.interceptor()))
}

func (e *identityEnv) userClient(c creds) syncrv1connect.UserServiceClient {
	return syncrv1connect.NewUserServiceClient(e.srv.Client(), e.srv.URL, connect.WithInterceptors(c.interceptor()))
}

// sessionCookie returns the one Set-Cookie of h, parsed.
func sessionCookie(t *testing.T, h http.Header) *http.Cookie {
	t.Helper()
	values := h.Values("Set-Cookie")
	if len(values) != 1 {
		t.Fatalf("Set-Cookie headers %q, want one", values)
	}
	c, err := http.ParseSetCookie(values[0])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// completeSetup creates the first admin ("admin") and returns their
// session credentials.
func (e *identityEnv) completeSetup() creds {
	e.t.Helper()
	resp, err := e.setupClient(creds{csrf: true}).CompleteSetup(e.t.Context(), connect.NewRequest(&syncrv1.CompleteSetupRequest{
		SetupCode: idSetupCode, Username: "admin", Password: idPassword,
	}))
	if err != nil {
		e.t.Fatal(err)
	}
	c := sessionCookie(e.t, resp.Header())
	return creds{cookie: c.Name + "=" + c.Value, csrf: true}
}

// login logs username in and returns its session credentials.
func (e *identityEnv) login(username, pw string) creds {
	e.t.Helper()
	resp, err := e.authClient(creds{csrf: true}).Login(e.t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: username, Password: pw}))
	if err != nil {
		e.t.Fatalf("Login(%s): %v", username, err)
	}
	c := sessionCookie(e.t, resp.Header())
	return creds{cookie: c.Name + "=" + c.Value, csrf: true}
}

// addUser creates a user through the API as admin.
func (e *identityEnv) addUser(admin creds, username string, role syncrv1.UserRole) *syncrv1.User {
	e.t.Helper()
	resp, err := e.userClient(admin).CreateUser(e.t.Context(), connect.NewRequest(&syncrv1.CreateUserRequest{Username: username, Password: idPassword, Role: role}))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.Msg.GetUser()
}

func (e *identityEnv) getSession(c creds) (*syncrv1.GetSessionResponse, error) {
	resp, err := e.authClient(c).GetSession(e.t.Context(), connect.NewRequest(&syncrv1.GetSessionRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestSessionCookieModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		opts    identityOptions
		secure  bool
		cookieN string
	}{
		{"auto over http", identityOptions{}, false, SessionCookie},
		{"auto with https public url", identityOptions{cookies: CookieConfig{PublicURL: "HTTPS://syncr.example"}}, true, SessionCookieSecure},
		{"auto with http public url", identityOptions{cookies: CookieConfig{Secure: CookieSecureAuto, PublicURL: "http://syncr.lan"}}, false, SessionCookie},
		{"auto over tls", identityOptions{tls: true}, true, SessionCookieSecure},
		{"always", identityOptions{cookies: CookieConfig{Secure: CookieSecureAlways}}, true, SessionCookieSecure},
		{"never, even over tls", identityOptions{cookies: CookieConfig{Secure: CookieSecureNever}, tls: true}, false, SessionCookie},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newIdentityEnv(t, tt.opts)
			status, err := e.setupClient(creds{}).GetSetupStatus(t.Context(), connect.NewRequest(&syncrv1.GetSetupStatusRequest{}))
			if err != nil || !status.Msg.GetSetupRequired() {
				t.Fatalf("GetSetupStatus = (%v, %v)", status, err)
			}
			resp, err := e.setupClient(creds{csrf: true}).CompleteSetup(t.Context(), connect.NewRequest(&syncrv1.CompleteSetupRequest{
				SetupCode: strings.ToLower(idSetupCode), Username: "admin", DisplayName: "Admin", Password: idPassword,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if u := resp.Msg.GetUser(); u.GetUsername() != "admin" || u.GetRole() != syncrv1.UserRole_USER_ROLE_ADMIN || u.GetVersion() != 1 {
				t.Errorf("user = %v", u)
			}
			c := sessionCookie(t, resp.Header())
			if c.Name != tt.cookieN || c.Secure != tt.secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode ||
				c.Path != "/" || c.Domain != "" || c.MaxAge != int(thirtyDays/time.Second) || len(c.Value) != 43 {
				t.Errorf("cookie = %+v, want %s, secure %v, HttpOnly, SameSite=Strict, Path=/, Max-Age 30 days", c, tt.cookieN, tt.secure)
			}
			if got := resp.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}

			session := creds{cookie: c.Name + "=" + c.Value, csrf: true}
			got, err := e.getSession(session)
			if err != nil {
				t.Fatal(err)
			}
			if got.GetMethod() != syncrv1.AuthMethod_AUTH_METHOD_SESSION || got.GetUser().GetUsername() != "admin" ||
				!got.GetExpiresAt().AsTime().Equal(e.clock.Now().Add(thirtyDays)) {
				t.Errorf("GetSession = %v", got)
			}
			// The cookie of the other mode is not read.
			other := SessionCookie
			if c.Name == SessionCookie {
				other = SessionCookieSecure
			}
			_, err = e.getSession(creds{cookie: other + "=" + c.Value, csrf: true})
			wantCode(t, err, connect.CodeUnauthenticated)
		})
	}
}

func TestCSRFHeaderIsRequired(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	// Setup and login without the header are refused before any check.
	body := fmt.Sprintf(`{"setupCode":%q,"username":"admin","password":%q}`, idSetupCode, idPassword)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+syncrv1connect.SetupServiceCompleteSetupProcedure, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("CompleteSetup without %s: %d, Set-Cookie %q", CSRFHeader, resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	for _, value := range []string{"", "0", "true"} {
		client := syncrv1connect.NewSetupServiceClient(e.srv.Client(), e.srv.URL, connect.WithInterceptors(headerInterceptor(CSRFHeader, value)))
		_, err := client.CompleteSetup(t.Context(), connect.NewRequest(&syncrv1.CompleteSetupRequest{SetupCode: idSetupCode, Username: "admin", Password: idPassword}))
		wantCode(t, err, connect.CodeInvalidArgument)
	}
	if req, _ := e.svc.SetupRequired(t.Context()); !req {
		t.Fatal("setup without the CSRF header created the admin")
	}

	admin := e.completeSetup()
	_, err = e.authClient(creds{}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "admin", Password: idPassword}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// A cookie without the header is no credential at all.
	noHeader := creds{cookie: admin.cookie}
	_, err = e.getSession(noHeader)
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.userClient(noHeader).ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	if _, err := e.getSession(admin); err != nil {
		t.Errorf("with the header: %v", err)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "without the "+CSRFHeader) {
		t.Errorf("the refusal was not logged: %s", logs)
	}
}

func TestLoginAndLogout(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	e.completeSetup()
	_, err := e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "admin", Password: idPassword2}))
	wantCode(t, err, connect.CodeUnauthenticated)
	if err != nil && !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("message: %v", err)
	}
	_, err = e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "nobody", Password: idPassword}))
	wantCode(t, err, connect.CodeUnauthenticated)

	resp, err := e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "ADMIN", Password: idPassword}))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.GetUser().GetLastLoginAt().IsValid() {
		t.Errorf("user = %v", resp.Msg.GetUser())
	}
	c := sessionCookie(t, resp.Header())
	session := creds{cookie: c.Name + "=" + c.Value, csrf: true}

	out, err := e.authClient(session).Logout(t.Context(), connect.NewRequest(&syncrv1.LogoutRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	cleared := sessionCookie(t, out.Header())
	if cleared.Name != SessionCookie || cleared.Value != "" || cleared.MaxAge != -1 || !cleared.HttpOnly || cleared.Path != "/" {
		t.Errorf("Logout cookie = %+v, want the session cookie deleted", cleared)
	}
	_, err = e.getSession(session)
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.authClient(session).Logout(t.Context(), connect.NewRequest(&syncrv1.LogoutRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestLoginRateLimitIsGeneric(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{loginIPBurst: 2})
	e.completeSetup() // costs one per-IP attempt
	_, err := e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "admin", Password: idPassword2}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "admin", Password: idPassword}))
	wantCode(t, err, connect.CodeResourceExhausted)
	if err != nil && (strings.Contains(err.Error(), "client_ip") || !strings.Contains(err.Error(), "try again later")) {
		t.Errorf("message: %v", err)
	}
	e.clock.Advance(time.Minute)
	if _, err := e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "admin", Password: idPassword})); err != nil {
		t.Errorf("after a refill: %v", err)
	}
}

func TestSetupErrors(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	_, err := e.setupClient(creds{csrf: true}).CompleteSetup(t.Context(), connect.NewRequest(&syncrv1.CompleteSetupRequest{
		SetupCode: "WRONG-WRONG-WRONG-WRONG", Username: "admin", Password: idPassword,
	}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := violations(t, err); len(v) != 1 || v[0].GetField() != "setup_code" {
		t.Errorf("violations = %v", v)
	}
	e.completeSetup()
	_, err = e.setupClient(creds{csrf: true}).CompleteSetup(t.Context(), connect.NewRequest(&syncrv1.CompleteSetupRequest{
		SetupCode: idSetupCode, Username: "second", Password: idPassword,
	}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	status, err := e.setupClient(creds{}).GetSetupStatus(t.Context(), connect.NewRequest(&syncrv1.GetSetupStatusRequest{}))
	if err != nil || status.Msg.GetSetupRequired() {
		t.Errorf("GetSetupStatus after setup = (%v, %v)", status, err)
	}
}

func TestBearerTokens(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	admin := e.completeSetup()
	created, err := e.authClient(admin).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "ci", Ttl: durationpb.New(time.Hour)}))
	if err != nil {
		t.Fatal(err)
	}
	secret := created.Msg.GetSecret()
	tok := created.Msg.GetToken()
	if !strings.HasPrefix(secret, users.APITokenPrefix) || tok.GetPrefix() != secret[:14] || tok.GetName() != "ci" ||
		!tok.GetExpiresAt().AsTime().Equal(e.clock.Now().Add(time.Hour)) || created.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("CreateAPIToken = %v (%q)", created.Msg, created.Header().Get("Cache-Control"))
	}

	// Bearer calls need no CSRF header and act with the owner's role.
	pat := bearer(secret)
	got, err := e.getSession(pat)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMethod() != syncrv1.AuthMethod_AUTH_METHOD_API_TOKEN || got.GetUser().GetUsername() != "admin" ||
		!got.GetExpiresAt().AsTime().Equal(tok.GetExpiresAt().AsTime()) {
		t.Errorf("GetSession(bearer) = %v", got)
	}
	if _, err := e.userClient(pat).ListUsers(t.Context(), connect.NewRequest(&syncrv1.ListUsersRequest{})); err != nil {
		t.Errorf("admin procedure with an admin's token: %v", err)
	}
	list, err := e.authClient(pat).ListAPITokens(t.Context(), connect.NewRequest(&syncrv1.ListAPITokensRequest{}))
	if err != nil || len(list.Msg.GetTokens()) != 1 || !list.Msg.GetTokens()[0].GetLastUsedAt().IsValid() {
		t.Errorf("ListAPITokens(bearer) = (%v, %v)", list, err)
	}

	// A token cannot mint or revoke tokens, or change the password.
	_, err = e.authClient(pat).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "more"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.authClient(pat).DeleteAPIToken(t.Context(), connect.NewRequest(&syncrv1.DeleteAPITokenRequest{TokenId: tok.GetId()}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = e.authClient(pat).ChangePassword(t.Context(), connect.NewRequest(&syncrv1.ChangePasswordRequest{CurrentPassword: idPassword, NewPassword: idPassword2}))
	wantCode(t, err, connect.CodePermissionDenied)
	// Logout with a token does nothing.
	out, err := e.authClient(pat).Logout(t.Context(), connect.NewRequest(&syncrv1.LogoutRequest{}))
	if err != nil || out.Header().Get("Set-Cookie") != "" {
		t.Errorf("Logout(bearer) = (%v, Set-Cookie %q)", err, out.Header().Get("Set-Cookie"))
	}

	// A wrong personal API token fails even with a valid cookie next to it;
	// other Authorization headers (a proxy's basic auth) are ignored, and
	// the cookie decides.
	for name, h := range map[string]string{
		"unknown pat":     "Bearer " + users.APITokenPrefix + strings.Repeat("A", 43),
		"truncated token": "Bearer " + secret[:len(secret)-1],
		"bearer lower":    "bearer " + users.APITokenPrefix + "x",
	} {
		_, err := e.getSession(creds{cookie: admin.cookie, csrf: true, authorization: h})
		if codeOf(t, err) != connect.CodeUnauthenticated {
			t.Errorf("%s: %v, want unauthenticated", name, err)
		}
	}
	for name, h := range map[string]string{
		"basic":          "Basic " + secret,
		"other bearer":   "Bearer " + strings.TrimPrefix(secret, users.APITokenPrefix),
		"session as pat": "Bearer " + strings.SplitN(admin.cookie, "=", 2)[1],
		"no token":       "Bearer",
	} {
		got, err := e.getSession(creds{cookie: admin.cookie, csrf: true, authorization: h})
		if err != nil || got.GetMethod() != syncrv1.AuthMethod_AUTH_METHOD_SESSION {
			t.Errorf("%s with a valid cookie: (%v, %v), want the session", name, got, err)
		}
		_, err = e.getSession(creds{authorization: h})
		wantCode(t, err, connect.CodeUnauthenticated)
	}

	// Sessions may delete tokens; then the token stops working.
	if _, err := e.authClient(admin).DeleteAPIToken(t.Context(), connect.NewRequest(&syncrv1.DeleteAPITokenRequest{TokenId: tok.GetId()})); err != nil {
		t.Fatal(err)
	}
	_, err = e.getSession(pat)
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.authClient(admin).DeleteAPIToken(t.Context(), connect.NewRequest(&syncrv1.DeleteAPITokenRequest{TokenId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = e.authClient(admin).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "x", Ttl: &durationpb.Duration{Seconds: 1, Nanos: -1}}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Expired tokens stop working.
	short, err := e.authClient(admin).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "short", Ttl: durationpb.New(time.Minute)}))
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	_, err = e.getSession(bearer(short.Msg.GetSecret()))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestSeveralSessionCookiesAreRefused(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	admin := e.completeSetup()
	second := e.login("admin", idPassword)
	for name, c := range map[string]creds{
		"one header":  {cookie: admin.cookie + "; " + second.cookie, csrf: true},
		"two headers": {cookie: admin.cookie, csrf: true},
	} {
		client := syncrv1connect.NewAuthServiceClient(e.srv.Client(), e.srv.URL,
			connect.WithInterceptors(c.interceptor(), addHeaderInterceptor("Cookie", second.cookie, name == "two headers")))
		_, err := client.GetSession(t.Context(), connect.NewRequest(&syncrv1.GetSessionRequest{}))
		wantCode(t, err, connect.CodeUnauthenticated)
	}
	// Unrelated cookies are fine.
	if _, err := e.getSession(creds{cookie: "theme=dark; " + admin.cookie + "; other=1", csrf: true}); err != nil {
		t.Errorf("with unrelated cookies: %v", err)
	}
}

// addHeaderInterceptor adds a request header value when enabled.
func addHeaderInterceptor(key, value string, enabled bool) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if enabled {
				req.Header().Add(key, value)
			}
			return next(ctx, req)
		}
	})
}

func TestChangePasswordThroughTheAPI(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	current := e.completeSetup()
	other := e.login("admin", idPassword)

	_, err := e.authClient(current).ChangePassword(t.Context(), connect.NewRequest(&syncrv1.ChangePasswordRequest{CurrentPassword: idPassword2, NewPassword: "a brand new password"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := violations(t, err); len(v) != 1 || v[0].GetField() != "current_password" {
		t.Errorf("violations = %v", v)
	}
	if _, err := e.getSession(current); err != nil {
		t.Fatalf("a wrong current password ended the session: %v", err)
	}
	if _, err := e.authClient(current).ChangePassword(t.Context(), connect.NewRequest(&syncrv1.ChangePasswordRequest{CurrentPassword: idPassword, NewPassword: "a brand new password"})); err != nil {
		t.Fatal(err)
	}
	if _, err := e.getSession(current); err != nil {
		t.Errorf("the current session was revoked: %v", err)
	}
	_, err = e.getSession(other)
	wantCode(t, err, connect.CodeUnauthenticated)
}

// TestCredentialsStayOutOfLogsAndErrors runs the flows that handle secrets
// and checks that none appears in the logs.
func TestCredentialsStayOutOfLogsAndErrors(t *testing.T) {
	t.Parallel()
	e := newIdentityEnv(t, identityOptions{})
	admin := e.completeSetup()
	created, err := e.authClient(admin).CreateAPIToken(t.Context(), connect.NewRequest(&syncrv1.CreateAPITokenRequest{Name: "ci"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.authClient(creds{csrf: true}).Login(t.Context(), connect.NewRequest(&syncrv1.LoginRequest{Username: "admin", Password: idPassword2}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.getSession(bearer(created.Msg.GetSecret() + "x"))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = e.getSession(creds{cookie: admin.cookie + "x", csrf: true})
	wantCode(t, err, connect.CodeUnauthenticated)
	if _, err := e.authClient(admin).ChangePassword(t.Context(), connect.NewRequest(&syncrv1.ChangePasswordRequest{CurrentPassword: idPassword, NewPassword: "a brand new password"})); err != nil {
		t.Fatal(err)
	}

	sessionToken := strings.SplitN(admin.cookie, "=", 2)[1]
	secretPart := strings.TrimPrefix(created.Msg.GetSecret(), users.APITokenPrefix)
	logs := e.logs.String()
	for name, secret := range map[string]string{
		"password": idPassword, "wrong password": idPassword2, "new password": "a brand new password",
		"setup code": "PQRS2", "session token": sessionToken, "api token": secretPart,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the %s", name)
		}
	}
	if !strings.Contains(logs, "api authentication failed") {
		t.Errorf("failures were not logged: %s", logs)
	}
}

// fakeVerifier is a CredentialVerifier that records what it was asked.
type fakeVerifier struct {
	session, token string
	err            error
}

func (f *fakeVerifier) AuthenticateSession(_ context.Context, token string) (domain.Actor, users.Session, error) {
	f.session = token
	if f.err != nil {
		return domain.Actor{}, users.Session{}, f.err
	}
	return testUser, users.Session{ExpiresAt: time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeVerifier) AuthenticateAPIToken(_ context.Context, token string) (domain.Actor, users.APIToken, error) {
	f.token = token
	if f.err != nil {
		return domain.Actor{}, users.APIToken{}, f.err
	}
	return testAdmin, users.APIToken{ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func TestSessionAuthUnit(t *testing.T) {
	t.Parallel()
	if _, err := NewSessionAuth(nil, CookieConfig{}); err == nil {
		t.Error("NewSessionAuth(nil) succeeded")
	}
	if _, err := NewSessionAuth(&fakeVerifier{}, CookieConfig{Secure: "sometimes"}); err == nil {
		t.Error("NewSessionAuth(unknown mode) succeeded")
	}
	const pat = "syncr_pat_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	csrf := http.CanonicalHeaderKey(CSRFHeader) // as a server sees it
	tests := []struct {
		name         string
		header       http.Header
		method       AuthMethod
		session, tok string
		// unauth is part of the expected ErrUnauthenticated reason.
		unauth string
	}{
		{name: "nothing", header: http.Header{}, unauth: "no credentials"},
		{name: "cookie and csrf", header: http.Header{"Cookie": {"syncr_session=sesstok123"}, csrf: {"1"}}, method: AuthSession, session: "sesstok123"},
		{name: "cookie without csrf", header: http.Header{"Cookie": {"syncr_session=sesstok123"}}, unauth: "without the"},
		{name: "cookie with wrong csrf", header: http.Header{"Cookie": {"syncr_session=sesstok123"}, csrf: {"yes"}}, unauth: "without the"},
		{name: "secure cookie in plain mode", header: http.Header{"Cookie": {"__Host-syncr_session=sesstok123"}, csrf: {"1"}}, unauth: "no credentials"},
		{name: "bearer", header: http.Header{"Authorization": {"Bearer " + pat}}, method: AuthToken, tok: pat},
		{name: "bearer lower case", header: http.Header{"Authorization": {"bearer  " + pat + " "}}, method: AuthToken, tok: pat},
		{name: "bearer wins over cookie", header: http.Header{"Authorization": {"Bearer " + pat}, "Cookie": {"syncr_session=sesstok123"}, csrf: {"1"}}, method: AuthToken, tok: pat},
		{name: "two authorization headers", header: http.Header{"Authorization": {"Bearer " + pat, "Bearer " + pat}}, unauth: "several"},
		{name: "token next to basic", header: http.Header{"Authorization": {"Basic abc", "Bearer " + pat}}, unauth: "several"},
		{name: "basic", header: http.Header{"Authorization": {"Basic abc"}}, unauth: "not a personal API token"},
		{name: "foreign bearer", header: http.Header{"Authorization": {"Bearer ghp_abc"}}, unauth: "not a personal API token"},
		{name: "basic next to a cookie", header: http.Header{"Authorization": {"Basic abc"}, "Cookie": {"syncr_session=sesstok123"}, csrf: {"1"}},
			method: AuthSession, session: "sesstok123"},
	}
	for _, tt := range tests {
		v := &fakeVerifier{}
		a, err := NewSessionAuth(v, CookieConfig{Secure: CookieSecureNever})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := a.Authenticate(t.Context(), tt.header, connect.Spec{})
		if tt.unauth != "" {
			if !errors.Is(err, domain.ErrUnauthenticated) || !strings.Contains(err.Error(), tt.unauth) {
				t.Errorf("%s: %v, want unauthenticated (%s)", tt.name, err, tt.unauth)
			}
			if v.session != "" || v.token != "" {
				t.Errorf("%s: the verifier was consulted", tt.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		c, ok := CallerFrom(ctx)
		if !ok || c.Method != tt.method || v.session != tt.session || v.token != tt.tok {
			t.Errorf("%s: caller %+v (%v), verifier saw session %q token %q", tt.name, c, ok, v.session, v.token)
		}
		switch tt.method {
		case AuthSession:
			if c.SessionToken.Reveal() != tt.session || c.Actor != testUser || !c.ExpiresAt.Equal(time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("%s: session caller %+v", tt.name, c)
			}
			if strings.Contains(fmt.Sprintf("%+v", c), tt.session) {
				t.Errorf("%s: the caller prints its token: %+v", tt.name, c)
			}
		case AuthToken:
			if !c.SessionToken.IsZero() || c.Actor != testAdmin || c.ExpiresAt.Year() != 2027 {
				t.Errorf("%s: token caller %+v", tt.name, c)
			}
		}
	}

	// Verifier failures pass through: unauthenticated ones as such, others
	// (store outages) as errors the guard reports as Internal.
	for _, verr := range []error{fmt.Errorf("x: %w", domain.ErrUnauthenticated), errors.New("store down")} {
		a, _ := NewSessionAuth(&fakeVerifier{err: verr}, CookieConfig{})
		_, err := a.Authenticate(t.Context(), http.Header{"Cookie": {"syncr_session=sesstok123"}, csrf: {"1"}}, connect.Spec{})
		if !errors.Is(err, verr) || errors.Is(err, domain.ErrUnauthenticated) != errors.Is(verr, domain.ErrUnauthenticated) {
			t.Errorf("verifier error %v became %v", verr, err)
		}
	}
}

func TestCookieConfig(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		c         CookieConfig
		tls, want bool
	}{
		{CookieConfig{}, false, false},
		{CookieConfig{}, true, true},
		{CookieConfig{PublicURL: "https://x"}, false, true},
		{CookieConfig{PublicURL: " https://x"}, false, true},
		{CookieConfig{PublicURL: "http://x"}, false, false},
		{CookieConfig{Secure: CookieSecureAlways}, false, true},
		{CookieConfig{Secure: CookieSecureNever, PublicURL: "https://x"}, true, false},
	} {
		if got := tt.c.SecureFor(tt.tls); got != tt.want {
			t.Errorf("%+v.SecureFor(%v) = %v", tt.c, tt.tls, got)
		}
	}
	if (CookieConfig{}).Name(true) != "__Host-syncr_session" || (CookieConfig{}).Name(false) != "syncr_session" {
		t.Error("cookie names")
	}
	if ClientIPFunc(nil).resolve("192.0.2.1:5555", nil) != "192.0.2.1" || ClientIPFunc(nil).resolve("pipe", nil) != "pipe" {
		t.Error("the default client IP is the peer's host")
	}
	f := ClientIPFunc(func(string, http.Header) string { return "198.51.100.1" })
	if f.resolve("192.0.2.1:5555", nil) != "198.51.100.1" {
		t.Error("the configured client IP function is not used")
	}
}
