package postgres

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// Config configures Open.
type Config struct {
	// URL is a libpq-style connection string: a postgres:// or
	// postgresql:// URL, or a "host=... dbname=..." key/value DSN. It
	// usually carries the password, so it is a redact.Secret and never
	// appears in logs or error messages.
	URL redact.Secret
	// MaxConns is the maximum number of pooled connections.
	MaxConns int32
	// MinConns is the number of connections the pool keeps open; 0 opens
	// them on demand.
	MinConns int32
	// MaxConnLifetime recycles a connection after this long, so a restarted
	// or rebalanced server is picked up; 0 means forever.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime closes a connection idle for this long; 0 means never.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod is how often the pool checks idle connections.
	HealthCheckPeriod time.Duration
	// ConnectTimeout bounds establishing one connection and Open's first
	// ping.
	ConnectTimeout time.Duration
}

// DefaultConfig returns the pool settings used when nothing is overridden.
// URL is empty and must be set.
func DefaultConfig() Config {
	return Config{
		MaxConns:          10,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   10 * time.Minute,
		HealthCheckPeriod: time.Minute,
		ConnectTimeout:    10 * time.Second,
	}
}

// Validate reports every invalid setting. It checks the pool settings and
// that URL is present; the connection string itself is parsed by Open. Error
// messages never contain the URL.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.URL.Reveal()) == "" {
		errs = append(errs, errors.New("postgres: url is required"))
	}
	if c.MaxConns < 1 {
		errs = append(errs, fmt.Errorf("postgres: max conns must be at least 1, got %d", c.MaxConns))
	}
	if c.MinConns < 0 || c.MinConns > c.MaxConns {
		errs = append(errs, fmt.Errorf("postgres: min conns must be between 0 and max conns (%d), got %d", c.MaxConns, c.MinConns))
	}
	if c.MaxConnLifetime < 0 {
		errs = append(errs, errors.New("postgres: max conn lifetime must not be negative"))
	}
	if c.MaxConnIdleTime < 0 {
		errs = append(errs, errors.New("postgres: max conn idle time must not be negative"))
	}
	if c.HealthCheckPeriod <= 0 {
		errs = append(errs, errors.New("postgres: health check period must be positive"))
	}
	if c.ConnectTimeout <= 0 {
		errs = append(errs, errors.New("postgres: connect timeout must be positive"))
	}
	return errors.Join(errs...)
}
