package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// CookieSecure selects when the session cookie is marked Secure
// (auth.cookie_secure).
type CookieSecure string

// Cookie security modes.
const (
	// CookieSecureAuto: Secure when the request came over TLS or
	// CookieConfig.PublicURL is https. The zero value means auto.
	CookieSecureAuto CookieSecure = "auto"
	// CookieSecureAlways: always Secure.
	CookieSecureAlways CookieSecure = "always"
	// CookieSecureNever: never Secure (plain-HTTP LAN setups).
	CookieSecureNever CookieSecure = "never"
)

// Session cookie names. The "__Host-" prefix makes browsers accept the
// cookie only if it is Secure, has Path=/ and no Domain: no other host,
// sibling subdomain or plain-HTTP response can set or shadow it.
const (
	SessionCookieSecure = "__Host-syncr_session"
	SessionCookie       = "syncr_session"
)

// CookieConfig configures the session cookie set by Login and
// CompleteSetup: SessionCookieSecure when Secure, SessionCookie otherwise;
// HttpOnly, SameSite=Strict, Path=/, Max-Age = the session's remaining
// lifetime. SessionAuth reads the same name.
type CookieConfig struct {
	// Secure is the security mode; "" means CookieSecureAuto.
	Secure CookieSecure
	// PublicURL is the controller's public base URL (http.public_url), if
	// configured.
	PublicURL string
}

// Validate reports an unknown Secure mode.
func (c CookieConfig) Validate() error {
	switch c.Secure {
	case "", CookieSecureAuto, CookieSecureAlways, CookieSecureNever:
		return nil
	}
	return fmt.Errorf("unknown cookie security mode %q (want auto, always or never)", string(c.Secure))
}

// SecureFor reports whether the cookie of a request is Secure; tls tells
// whether the request came over TLS. The composition root can call it with
// its listener's TLS state to warn when sessions would travel in clear.
func (c CookieConfig) SecureFor(tls bool) bool {
	switch c.Secure {
	case CookieSecureAlways:
		return true
	case CookieSecureNever:
		return false
	default:
		return tls || strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.PublicURL)), "https://")
	}
}

// Name returns the cookie's name in the given Secure mode.
func (c CookieConfig) Name(secure bool) string {
	if secure {
		return SessionCookieSecure
	}
	return SessionCookie
}

// sessionCookie returns the cookie carrying token for maxAge, for a
// response to the request of ctx.
func (c CookieConfig) sessionCookie(ctx context.Context, token string, maxAge time.Duration) *http.Cookie {
	secure := c.SecureFor(requestTLS(ctx))
	return &http.Cookie{ //nolint:gosec // G124: Secure follows auth.cookie_secure (plain-HTTP setups need it off); HttpOnly and SameSite=Strict are constant
		Name: c.Name(secure), Value: token, Path: "/",
		MaxAge:   max(1, int(maxAge/time.Second)),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// clearCookie returns the cookie that deletes the session cookie.
func (c CookieConfig) clearCookie(ctx context.Context) *http.Cookie {
	cookie := c.sessionCookie(ctx, "", 0) //nolint:gosec // G124: see sessionCookie
	cookie.MaxAge = -1                    // Max-Age=0
	return cookie
}

// ClientIPFunc derives a request's client IP address from the TCP peer's
// address ("host:port") and the headers: clientip.Resolver.ClientIP, which
// trusts forwarding headers only from the configured proxies.
type ClientIPFunc func(remoteAddr string, header http.Header) string

// resolve returns the client IP; without a function, the peer's host.
func (f ClientIPFunc) resolve(remoteAddr string, header http.Header) string {
	if f != nil {
		return f(remoteAddr, header)
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// requireCSRF checks the CSRF header of the public calls that start a
// session (Login, CompleteSetup); SessionAuth checks it for cookie calls.
// Without it, a cross-site form could log the victim into an account of the
// attacker's.
func requireCSRF(h http.Header) error {
	if h.Get(CSRFHeader) != "1" {
		return invalid("missing header " + CSRFHeader + ": 1")
	}
	return nil
}

// noStore marks a response that carries a credential as uncacheable.
func noStore(h http.Header) { h.Set("Cache-Control", "no-store") }

// setupHandler implements syncr.v1.SetupService.
type setupHandler struct {
	syncrv1connect.UnimplementedSetupServiceHandler

	identity IdentityService
	cookies  CookieConfig
	clientIP ClientIPFunc
	errs     errorMapper
}

var _ syncrv1connect.SetupServiceHandler = (*setupHandler)(nil)

// GetSetupStatus tells whether the first admin still has to be created.
func (h *setupHandler) GetSetupStatus(ctx context.Context, _ *connect.Request[syncrv1.GetSetupStatusRequest]) (*connect.Response[syncrv1.GetSetupStatusResponse], error) {
	required, err := h.identity.SetupRequired(ctx)
	if err != nil {
		return nil, h.errs.toConnect(ctx, syncrv1connect.SetupServiceGetSetupStatusProcedure, "setup", err)
	}
	return connect.NewResponse(&syncrv1.GetSetupStatusResponse{SetupRequired: required}), nil
}

// CompleteSetup creates the first admin and sets their session cookie.
func (h *setupHandler) CompleteSetup(ctx context.Context, req *connect.Request[syncrv1.CompleteSetupRequest]) (*connect.Response[syncrv1.CompleteSetupResponse], error) {
	if err := requireCSRF(req.Header()); err != nil {
		return nil, err
	}
	m := req.Msg
	u, issued, err := h.identity.CompleteSetup(ctx, users.SetupParams{
		Code: m.GetSetupCode(), Username: m.GetUsername(), DisplayName: m.GetDisplayName(), Password: m.GetPassword(),
		ClientIP: h.clientIP.resolve(req.Peer().Addr, req.Header()), UserAgent: req.Header().Get("User-Agent"),
	})
	if err != nil {
		return nil, h.errs.toConnect(ctx, syncrv1connect.SetupServiceCompleteSetupProcedure, "user", err)
	}
	resp := connect.NewResponse(&syncrv1.CompleteSetupResponse{User: userToProto(u)})
	resp.Header().Add("Set-Cookie", h.cookies.sessionCookie(ctx, issued.Token, issued.MaxAge).String())
	noStore(resp.Header())
	return resp, nil
}

// authHandler implements syncr.v1.AuthService.
type authHandler struct {
	syncrv1connect.UnimplementedAuthServiceHandler

	identity IdentityService
	cookies  CookieConfig
	clientIP ClientIPFunc
	errs     errorMapper
}

var _ syncrv1connect.AuthServiceHandler = (*authHandler)(nil)

func (h *authHandler) fail(ctx context.Context, procedure, what string, err error) error {
	return h.errs.toConnect(ctx, procedure, what, err)
}

// Login checks the credentials and sets the session cookie. Wrong
// credentials, an unknown user and a disabled account are one generic
// Unauthenticated error; too many attempts are ResourceExhausted.
func (h *authHandler) Login(ctx context.Context, req *connect.Request[syncrv1.LoginRequest]) (*connect.Response[syncrv1.LoginResponse], error) {
	if err := requireCSRF(req.Header()); err != nil {
		return nil, err
	}
	u, issued, err := h.identity.Login(ctx, users.LoginParams{
		Username: req.Msg.GetUsername(), Password: req.Msg.GetPassword(),
		ClientIP: h.clientIP.resolve(req.Peer().Addr, req.Header()), UserAgent: req.Header().Get("User-Agent"),
	})
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceLoginProcedure, "user", err)
	}
	resp := connect.NewResponse(&syncrv1.LoginResponse{User: userToProto(u)})
	resp.Header().Add("Set-Cookie", h.cookies.sessionCookie(ctx, issued.Token, issued.MaxAge).String())
	noStore(resp.Header())
	return resp, nil
}

// Logout ends the caller's session and clears the cookie. With an API
// token it does nothing.
func (h *authHandler) Logout(ctx context.Context, _ *connect.Request[syncrv1.LogoutRequest]) (*connect.Response[syncrv1.LogoutResponse], error) {
	c, _ := CallerFrom(ctx)
	resp := connect.NewResponse(&syncrv1.LogoutResponse{})
	if c.Method != AuthSession {
		return resp, nil
	}
	if err := h.identity.Logout(ctx, c.SessionToken.Reveal()); err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceLogoutProcedure, "session", err)
	}
	resp.Header().Add("Set-Cookie", h.cookies.clearCookie(ctx).String())
	return resp, nil
}

// GetSession returns the caller, when their session or token stops working
// and how they authenticated.
func (h *authHandler) GetSession(ctx context.Context, _ *connect.Request[syncrv1.GetSessionRequest]) (*connect.Response[syncrv1.GetSessionResponse], error) {
	c, _ := CallerFrom(ctx)
	u, err := h.identity.Me(ctx, c.Actor)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceGetSessionProcedure, "user", err)
	}
	method := syncrv1.AuthMethod_AUTH_METHOD_SESSION
	if c.Method == AuthToken {
		method = syncrv1.AuthMethod_AUTH_METHOD_API_TOKEN
	}
	resp := connect.NewResponse(&syncrv1.GetSessionResponse{User: userToProto(u), ExpiresAt: ts(c.ExpiresAt), Method: method})
	noStore(resp.Header())
	return resp, nil
}

// ChangePassword sets a new password and ends the caller's other sessions
// (the policy table admits only session callers).
func (h *authHandler) ChangePassword(ctx context.Context, req *connect.Request[syncrv1.ChangePasswordRequest]) (*connect.Response[syncrv1.ChangePasswordResponse], error) {
	c, _ := CallerFrom(ctx)
	err := h.identity.ChangePassword(ctx, c.Actor, c.SessionToken.Reveal(), req.Msg.GetCurrentPassword(), req.Msg.GetNewPassword())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceChangePasswordProcedure, "user", err)
	}
	return connect.NewResponse(&syncrv1.ChangePasswordResponse{}), nil
}

// CreateAPIToken mints a token; its secret is in this response only.
func (h *authHandler) CreateAPIToken(ctx context.Context, req *connect.Request[syncrv1.CreateAPITokenRequest]) (*connect.Response[syncrv1.CreateAPITokenResponse], error) {
	ttl := req.Msg.GetTtl()
	if ttl != nil {
		if err := ttl.CheckValid(); err != nil {
			return nil, invalidField("ttl", "is not a valid duration")
		}
	}
	actor, _ := ActorFrom(ctx)
	t, secret, err := h.identity.CreateAPIToken(ctx, actor, req.Msg.GetName(), ttl.AsDuration())
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceCreateAPITokenProcedure, "api token", err)
	}
	resp := connect.NewResponse(&syncrv1.CreateAPITokenResponse{Token: apiTokenToProto(t), Secret: secret})
	noStore(resp.Header())
	return resp, nil
}

// ListAPITokens lists the caller's tokens, newest first.
func (h *authHandler) ListAPITokens(ctx context.Context, _ *connect.Request[syncrv1.ListAPITokensRequest]) (*connect.Response[syncrv1.ListAPITokensResponse], error) {
	actor, _ := ActorFrom(ctx)
	ts, err := h.identity.ListAPITokens(ctx, actor)
	if err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceListAPITokensProcedure, "api token", err)
	}
	resp := &syncrv1.ListAPITokensResponse{Tokens: make([]*syncrv1.APIToken, 0, len(ts))}
	for _, t := range ts {
		resp.Tokens = append(resp.Tokens, apiTokenToProto(t))
	}
	return connect.NewResponse(resp), nil
}

// DeleteAPIToken revokes one of the caller's tokens.
func (h *authHandler) DeleteAPIToken(ctx context.Context, req *connect.Request[syncrv1.DeleteAPITokenRequest]) (*connect.Response[syncrv1.DeleteAPITokenResponse], error) {
	id, err := parseAPITokenID(req.Msg.GetTokenId())
	if err != nil {
		return nil, err
	}
	actor, _ := ActorFrom(ctx)
	if err := h.identity.DeleteAPIToken(ctx, actor, id); err != nil {
		return nil, h.fail(ctx, syncrv1connect.AuthServiceDeleteAPITokenProcedure, "api token", err)
	}
	return connect.NewResponse(&syncrv1.DeleteAPITokenResponse{}), nil
}

func parseAPITokenID(s string) (domain.APITokenID, error) {
	if s == "" {
		return domain.APITokenID{}, invalidField("token_id", "is required")
	}
	id, err := domain.ParseAPITokenID(s)
	if err != nil || id.IsZero() {
		return domain.APITokenID{}, invalidField("token_id", "is not a valid ID")
	}
	return id, nil
}

func apiTokenToProto(t users.APIToken) *syncrv1.APIToken {
	return &syncrv1.APIToken{
		Id: t.ID.String(), Name: t.Name, Prefix: t.Prefix,
		CreatedAt: ts(t.CreatedAt), ExpiresAt: ts(t.ExpiresAt), LastUsedAt: ts(t.LastUsedAt),
	}
}
