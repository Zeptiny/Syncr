package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
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

// CookieConfig configures the session cookie set by Login and
// CompleteSetup: "__Host-syncr_session" when Secure, "syncr_session"
// otherwise; HttpOnly, SameSite=Strict, Path=/.
type CookieConfig struct {
	// Secure is the security mode; "" means CookieSecureAuto.
	Secure CookieSecure
	// PublicURL is the controller's public base URL (http.public_url), if
	// configured.
	PublicURL string
}

// setupHandler implements syncr.v1.SetupService.
//
// m3: pending track A: every RPC is Unimplemented until then.
type setupHandler struct {
	syncrv1connect.UnimplementedSetupServiceHandler

	identity IdentityService
	cookies  CookieConfig
	errs     errorMapper
}

var _ syncrv1connect.SetupServiceHandler = (*setupHandler)(nil)

// authHandler implements syncr.v1.AuthService.
//
// m3: pending track A: every RPC is Unimplemented until then.
type authHandler struct {
	syncrv1connect.UnimplementedAuthServiceHandler

	identity IdentityService
	cookies  CookieConfig
	errs     errorMapper
}

var _ syncrv1connect.AuthServiceHandler = (*authHandler)(nil)
