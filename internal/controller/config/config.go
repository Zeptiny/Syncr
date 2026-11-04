// Package config defines the controller's configuration file and
// environment variables.
//
// The file is YAML (see README.md for an example); every setting
// can be overridden by the SYNCR_* variable named in its env tag. Loading
// mechanics live in internal/platform/config.
//
// Allowed dependencies: the standard library and internal/platform packages.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// Config is the controller configuration.
type Config struct {
	HTTP    HTTP           `yaml:"http"`
	Log     logging.Config `yaml:"log"`
	Secrets Secrets        `yaml:"secrets"`
	API     API            `yaml:"api"`
	// Database configures the Postgres store.
	Database Database `yaml:"database"`
	// Dev holds development-only switches.
	Dev Dev `yaml:"dev"`
}

// Dev holds switches for demos and tests. Never use them in production.
type Dev struct {
	// MemoryStore runs the controller on an in-memory store instead of
	// Postgres: every worker, token and job is lost when the process
	// exits. It is refused together with database.url, and the controller
	// logs a loud warning when it is on.
	MemoryStore bool `yaml:"memory_store" env:"SYNCR_DEV_MEMORY_STORE"`
}

// Database configures the Postgres connection pool (internal/adapters/postgres).
type Database struct {
	// URL is the connection string, a postgres:// URL or a key/value DSN,
	// e.g. postgres://syncr:password@db:5432/syncr?sslmode=require. It is
	// empty until the controller is deployed against a database; Configured
	// reports whether it is set. It is a redact.Secret because it carries the
	// password: it is never printed or logged.
	URL redact.Secret `yaml:"url" env:"SYNCR_DATABASE_URL"`
	// MaxConns is the maximum number of pooled connections.
	MaxConns int32 `yaml:"max_conns" env:"SYNCR_DATABASE_MAX_CONNS"`
	// ConnectTimeout bounds opening one connection and the first ping at
	// startup.
	ConnectTimeout time.Duration `yaml:"connect_timeout" env:"SYNCR_DATABASE_CONNECT_TIMEOUT"`
}

// Configured reports whether a database URL is set.
func (d Database) Configured() bool { return strings.TrimSpace(d.URL.Reveal()) != "" }

// Secrets configures the encryption of secrets at rest (job specs, worker
// secrets) with AES-256-GCM (internal/adapters/secrets). Keys are 32 random
// bytes in standard base64, e.g. from `openssl rand -base64 32`. Losing the
// key makes every stored secret unreadable.
//
// To rotate: move the current key to previous_keys as "<key_id>:<key>", set
// a new key with a new key_id, restart; stored values are re-encrypted with
// the new key as they are rewritten, after which the old key can go.
type Secrets struct {
	// Key is the active master key. Required to serve (CheckServe).
	Key string `yaml:"key" env:"SYNCR_SECRETS_KEY"`
	// KeyID identifies Key inside ciphertexts (default 1).
	KeyID uint32 `yaml:"key_id" env:"SYNCR_SECRETS_KEY_ID"`
	// PreviousKeys are "<id>:<base64 key>" entries still accepted for
	// decryption (comma-separated in the environment).
	PreviousKeys []string `yaml:"previous_keys" env:"SYNCR_SECRETS_PREVIOUS_KEYS"`
}

// secretsKeySize is the AES-256 key size.
const secretsKeySize = 32

// HTTP configures the API/UI listener, which also serves the worker link
// (active workers' long polls and enrollment).
type HTTP struct {
	// Addr is the listen address, e.g. ":8080" or "127.0.0.1:8080".
	Addr string `yaml:"addr" env:"SYNCR_HTTP_ADDR"`
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" env:"SYNCR_HTTP_READ_HEADER_TIMEOUT"`
	// ReadTimeout and WriteTimeout bound reading a whole request and
	// writing its response; 0 disables them. Worker long polls and WatchJob
	// streams extend their own connection's deadlines, so these only limit
	// ordinary calls.
	ReadTimeout  time.Duration `yaml:"read_timeout" env:"SYNCR_HTTP_READ_TIMEOUT"`
	WriteTimeout time.Duration `yaml:"write_timeout" env:"SYNCR_HTTP_WRITE_TIMEOUT"`
	// IdleTimeout closes keep-alive connections idle this long. Keep it
	// above the workers' report interval (15 s) so connections are reused.
	IdleTimeout time.Duration `yaml:"idle_timeout" env:"SYNCR_HTTP_IDLE_TIMEOUT"`
	// ShutdownTimeout bounds graceful shutdown: in-flight requests get this
	// long to finish after SIGINT/SIGTERM.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" env:"SYNCR_HTTP_SHUTDOWN_TIMEOUT"`

	// TLSCertFile and TLSKeyFile make the controller serve HTTPS itself
	// (PEM certificate chain and private key), for setups without a
	// reverse proxy. Both or neither must be set; without them the
	// controller serves plain HTTP (and h2c) and expects a TLS-terminating
	// proxy in front of it.
	TLSCertFile string `yaml:"tls_cert_file" env:"SYNCR_HTTP_TLS_CERT_FILE"`
	TLSKeyFile  string `yaml:"tls_key_file" env:"SYNCR_HTTP_TLS_KEY_FILE"`

	// ClientIPHeader names the header a reverse proxy puts the client's IP
	// address in, e.g. "X-Forwarded-For" or "X-Real-IP". It is used (for
	// the enrollment rate limit) only on requests whose TCP peer is one of
	// TrustedProxies; otherwise the peer address counts. The proxy must
	// overwrite or append to the header, never pass a client's value on
	// unchecked.
	ClientIPHeader string `yaml:"client_ip_header" env:"SYNCR_HTTP_CLIENT_IP_HEADER"`
	// TrustedProxies are the addresses (CIDRs or single IPs) of the
	// reverse proxies allowed to set ClientIPHeader. Required with it.
	TrustedProxies []string `yaml:"trusted_proxies" env:"SYNCR_HTTP_TRUSTED_PROXIES"`
}

// TLS reports whether the controller serves HTTPS itself.
func (h HTTP) TLS() bool { return h.TLSCertFile != "" || h.TLSKeyFile != "" }

// Default returns the configuration used when nothing is overridden.
func Default() Config {
	return Config{
		HTTP: HTTP{
			Addr:              ":8080",
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Minute,
			WriteTimeout:      time.Minute,
			IdleTimeout:       2 * time.Minute,
			ShutdownTimeout:   15 * time.Second,
		},
		Log:     logging.DefaultConfig(),
		Secrets: Secrets{KeyID: 1},
		API:     DefaultAPI(),
		Database: Database{
			MaxConns:       10,
			ConnectTimeout: 10 * time.Second,
		},
	}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	return errors.Join(c.HTTP.validate(), c.Log.Validate(), c.Secrets.validate(), c.API.validate(), c.Database.validate())
}

func (h HTTP) validate() error {
	var errs []error
	if err := validateAddr(h.Addr); err != nil {
		errs = append(errs, fmt.Errorf("http.addr: %w", err))
	}
	if h.ReadHeaderTimeout <= 0 {
		errs = append(errs, errors.New("http.read_header_timeout must be positive"))
	}
	if h.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("http.shutdown_timeout must be positive"))
	}
	if h.ReadTimeout < 0 || h.WriteTimeout < 0 || h.IdleTimeout < 0 {
		errs = append(errs, errors.New("http.read_timeout, http.write_timeout and http.idle_timeout must not be negative"))
	}
	if (h.TLSCertFile == "") != (h.TLSKeyFile == "") {
		errs = append(errs, errors.New("http.tls_cert_file and http.tls_key_file must be set together"))
	}
	if _, err := ParseTrustedProxies(h.TrustedProxies); err != nil {
		errs = append(errs, err)
	}
	switch {
	case h.ClientIPHeader != "" && len(h.TrustedProxies) == 0:
		errs = append(errs, errors.New("http.client_ip_header needs http.trusted_proxies (the proxies allowed to set it)"))
	case h.ClientIPHeader == "" && len(h.TrustedProxies) > 0:
		errs = append(errs, errors.New("http.trusted_proxies needs http.client_ip_header (the header they set)"))
	case h.ClientIPHeader != "" && !validHeaderName(h.ClientIPHeader):
		errs = append(errs, fmt.Errorf("http.client_ip_header: %q is not a valid header name", h.ClientIPHeader))
	}
	return errors.Join(errs...)
}

// ParseTrustedProxies parses http.trusted_proxies: CIDRs ("10.0.0.0/8") or
// single addresses ("192.0.2.10", a /32 or /128).
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	var (
		out  []netip.Prefix
		errs []error
	)
	for i, raw := range list {
		raw = strings.TrimSpace(raw)
		if p, err := netip.ParsePrefix(raw); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(raw); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		errs = append(errs, fmt.Errorf("http.trusted_proxies[%d]: %q is not an IP address or CIDR", i, raw))
	}
	return out, errors.Join(errs...)
}

// validHeaderName reports whether s is an HTTP header field name (an RFC 9110
// token).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		isTokenChar := unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("!#$%&'*+-.^_`|~", r)
		if r > unicode.MaxASCII || !isTokenChar {
			return false
		}
	}
	return true
}

// CheckServe reports what the controller needs to serve beyond a valid
// configuration: a secrets key, and a database (or the explicit
// development memory store). Validate deliberately accepts a configuration
// without them, so that commands that do not serve (-version) work.
func (c *Config) CheckServe() error {
	var errs []error
	if c.Secrets.Key == "" {
		errs = append(errs, errors.New("secrets.key is required (SYNCR_SECRETS_KEY): 32 random bytes in base64, e.g. from `openssl rand -base64 32`; "+
			"it encrypts job specs and worker secrets at rest, so back it up"))
	}
	switch {
	case c.Dev.MemoryStore && c.Database.Configured():
		errs = append(errs, errors.New("dev.memory_store and database.url are both set; choose one"))
	case !c.Dev.MemoryStore && !c.Database.Configured():
		errs = append(errs, errors.New("database.url is required (SYNCR_DATABASE_URL), e.g. postgres://syncr:<password>@db:5432/syncr; "+
			"for a throw-away demo without Postgres set dev.memory_store: true (SYNCR_DEV_MEMORY_STORE=true)"))
	}
	return errors.Join(errs...)
}

func (d Database) validate() error {
	var errs []error
	if d.Configured() {
		if err := validateDatabaseURL(d.URL.Reveal()); err != nil {
			errs = append(errs, fmt.Errorf("database.url: %w", err))
		}
	}
	if d.MaxConns < 1 {
		errs = append(errs, errors.New("database.max_conns must be at least 1"))
	}
	if d.ConnectTimeout <= 0 {
		errs = append(errs, errors.New("database.connect_timeout must be positive"))
	}
	return errors.Join(errs...)
}

// validateDatabaseURL accepts a postgres:// or postgresql:// URL with a host
// or socket path, or a libpq key/value DSN. It never echoes the value: the
// string carries the password, and url.Parse errors quote their input.
func validateDatabaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if !strings.Contains(raw, "=") {
			return errors.New("want a postgres:// URL or a key=value connection string")
		}
		return nil
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("not a valid URL")
	case u.Scheme != "postgres" && u.Scheme != "postgresql":
		return errors.New("scheme must be postgres or postgresql")
	case u.Host == "" && u.Query().Get("host") == "":
		return errors.New("missing host")
	}
	return nil
}

func (s Secrets) validate() error {
	var errs []error
	if s.Key == "" {
		if len(s.PreviousKeys) > 0 {
			errs = append(errs, errors.New("secrets.previous_keys needs secrets.key"))
		}
		return errors.Join(errs...)
	}
	if err := validateKey(s.Key); err != nil {
		errs = append(errs, fmt.Errorf("secrets.key: %w", err))
	}
	seen := map[uint64]bool{uint64(s.KeyID): true}
	for i, spec := range s.PreviousKeys {
		idStr, key, ok := strings.Cut(spec, ":")
		id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 32)
		switch {
		case !ok || err != nil:
			errs = append(errs, fmt.Errorf("secrets.previous_keys[%d]: want <id>:<base64 key>", i))
		case seen[id]:
			errs = append(errs, fmt.Errorf("secrets.previous_keys[%d]: key id %d is used twice", i, id))
		default:
			seen[id] = true
			if err := validateKey(key); err != nil {
				errs = append(errs, fmt.Errorf("secrets.previous_keys[%d]: %w", i, err))
			}
		}
	}
	return errors.Join(errs...)
}

// validateKey checks for exactly 32 bytes of base64 (any alphabet, padded
// or not, as internal/adapters/secrets.ParseKey accepts). It never echoes
// the key.
func validateKey(key string) error {
	key = strings.TrimSpace(key)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(key); err == nil {
			if len(b) != secretsKeySize {
				return fmt.Errorf("key is %d bytes, want %d", len(b), secretsKeySize)
			}
			return nil
		}
	}
	return errors.New("key is not valid base64")
}

func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || (n == 0 && port != "0") {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}
