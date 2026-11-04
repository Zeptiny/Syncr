package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// MinAdminTokenLen is the shortest accepted api.admin_token.
const MinAdminTokenLen = 32

// API configures the controller's public API (internal/adapters/api).
type API struct {
	// AdminToken is the provisional shared secret protecting
	// WorkerAdminService and JobService until users and sessions arrive in
	// Milestone 3. Clients send it as "Authorization: Bearer <token>". It
	// must be at least MinAdminTokenLen characters without whitespace, e.g.
	// from `openssl rand -base64 32`.
	//
	// When empty, those services are NOT served at all (fail closed) and the
	// controller logs a warning at startup; SystemService and /healthz stay
	// available.
	AdminToken redact.Secret `yaml:"admin_token" env:"SYNCR_ADMIN_TOKEN"`
	// RequestTimeout bounds unary API calls.
	RequestTimeout time.Duration `yaml:"request_timeout" env:"SYNCR_API_REQUEST_TIMEOUT"`
	// WatchMaxDuration ends a WatchJob stream after this long; clients
	// re-watch (the first message is always the current job).
	WatchMaxDuration time.Duration `yaml:"watch_max_duration" env:"SYNCR_API_WATCH_MAX_DURATION"`
	// WatchKeepalive resends a watched job's snapshot when nothing else was
	// sent for this long, so idle proxies keep the stream open.
	WatchKeepalive time.Duration `yaml:"watch_keepalive" env:"SYNCR_API_WATCH_KEEPALIVE"`
}

// DefaultAPI returns the default API configuration (no admin token).
func DefaultAPI() API {
	return API{
		RequestTimeout:   30 * time.Second,
		WatchMaxDuration: time.Hour,
		WatchKeepalive:   30 * time.Second,
	}
}

func (a API) validate() error {
	var errs []error
	if tok := a.AdminToken.Reveal(); tok != "" {
		switch {
		case len(tok) < MinAdminTokenLen:
			errs = append(errs, fmt.Errorf("api.admin_token must be at least %d characters (it is %d)", MinAdminTokenLen, len(tok)))
		case strings.ContainsFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII }):
			errs = append(errs, errors.New("api.admin_token must be printable ASCII without whitespace"))
		}
	}
	if a.RequestTimeout <= 0 {
		errs = append(errs, errors.New("api.request_timeout must be positive"))
	}
	if a.WatchMaxDuration <= 0 {
		errs = append(errs, errors.New("api.watch_max_duration must be positive"))
	}
	if a.WatchKeepalive <= 0 {
		errs = append(errs, errors.New("api.watch_keepalive must be positive"))
	}
	return errors.Join(errs...)
}
