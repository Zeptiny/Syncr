package config

import (
	"errors"
	"time"
)

// API configures the controller's public API (internal/adapters/api).
type API struct {
	// AdminToken is the provisional shared admin token of Milestone 2
	// (api.admin_token, SYNCR_ADMIN_TOKEN). It was removed in favor of
	// users and personal API tokens: setting it has no effect, and the
	// controller logs a warning (see Config.RemovedSettings). Its value is
	// never kept.
	AdminToken Removed `yaml:"admin_token" env:"SYNCR_ADMIN_TOKEN"`
	// RequestTimeout bounds unary API calls.
	RequestTimeout time.Duration `yaml:"request_timeout" env:"SYNCR_API_REQUEST_TIMEOUT"`
	// WatchMaxDuration ends a WatchJob stream after this long; clients
	// re-watch (the first message is always the current job).
	WatchMaxDuration time.Duration `yaml:"watch_max_duration" env:"SYNCR_API_WATCH_MAX_DURATION"`
	// WatchKeepalive resends a watched job's snapshot when nothing else was
	// sent for this long, so idle proxies keep the stream open.
	WatchKeepalive time.Duration `yaml:"watch_keepalive" env:"SYNCR_API_WATCH_KEEPALIVE"`
}

// DefaultAPI returns the default API configuration.
func DefaultAPI() API {
	return API{
		RequestTimeout:   30 * time.Second,
		WatchMaxDuration: time.Hour,
		WatchKeepalive:   30 * time.Second,
	}
}

func (a API) validate() error {
	var errs []error
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

// Removed is the type of a setting that no longer has any effect. Loading
// it (from the file or the environment) records only that it was set,
// never its value, so a removed secret cannot leak through the
// configuration; the controller warns about it at start instead of
// refusing a configuration written for an older version.
type Removed bool

// UnmarshalText implements encoding.TextUnmarshaler: any value marks the
// setting as set and is discarded.
func (r *Removed) UnmarshalText([]byte) error {
	*r = true
	return nil
}

// RemovedSetting describes a removed setting that is still configured.
type RemovedSetting struct {
	// Key and Env name the setting in the file and the environment.
	Key, Env string
	// Instead says what replaces it.
	Instead string
}

// RemovedSettings returns the removed settings that are still configured,
// for the controller to warn about at start.
func (c *Config) RemovedSettings() []RemovedSetting {
	var out []RemovedSetting
	if c.API.AdminToken {
		out = append(out, RemovedSetting{Key: "api.admin_token", Env: "SYNCR_ADMIN_TOKEN",
			Instead: "removed; create an API token in the UI (Settings, API tokens) and send it as \"Authorization: Bearer syncr_pat_...\""})
	}
	return out
}
