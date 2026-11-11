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
	// Auth configures users' sessions and the first-run setup.
	Auth Auth `yaml:"auth"`
	// Workers configures what the controller tells new workers and when it
	// gives up on an unreachable one.
	Workers Workers `yaml:"workers"`
	// Database configures the Postgres store.
	Database Database `yaml:"database"`
	// Schedules configures the scheduler.
	Schedules Schedules `yaml:"schedules"`
	// Jobs configures job time limits and the retention of job samples.
	Jobs Jobs `yaml:"jobs"`
	// Notify configures notifications and their network policy.
	Notify Notify `yaml:"notify"`
	// Probe bounds the short operations workers run for the UI.
	Probe Probe `yaml:"probe"`
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
	// Seed fills the in-memory store with fixed demo data at start (users
	// admin and alice, workers, remotes, schedules, contacts, deliveries
	// and a month of jobs; internal/devseed), for screenshots and UI
	// development. It needs MemoryStore. Notification channels only log
	// what they would send (notify.DryRun). The demo passwords are public:
	// never use it on a reachable controller.
	Seed bool `yaml:"seed" env:"SYNCR_DEV_SEED"`
}

func (d Dev) validate() error {
	if d.Seed && !d.MemoryStore {
		return errors.New("dev.seed needs dev.memory_store (the demo data is only ever written to the in-memory store)")
	}
	return nil
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

// Secrets configures the encryption of secrets at rest (remote credentials,
// worker secrets) with AES-256-GCM (internal/adapters/secrets). Keys are 32
// random bytes in standard base64, e.g. from `openssl rand -base64 32`.
// Losing the key makes every stored secret unreadable.
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

	// PublicURL is the controller's base URL as browsers and workers reach
	// it, e.g. "https://syncr.example.com" behind a reverse proxy. It goes
	// into the worker install commands (without it, the web UI's origin is
	// used), and with "https://" the session cookie is Secure
	// (auth.cookie_secure: auto). Workers accept https, or http on a
	// loopback address only. Empty when unset.
	PublicURL string `yaml:"public_url" env:"SYNCR_PUBLIC_URL"`
}

// TLS reports whether the controller serves HTTPS itself.
func (h HTTP) TLS() bool { return h.TLSCertFile != "" || h.TLSKeyFile != "" }

// Auth configures sessions and the first-run setup (internal/app/users,
// the session cookie of internal/adapters/api).
type Auth struct {
	// CookieSecure decides whether the session cookie is Secure (sent over
	// HTTPS only): "auto" (when the request came over TLS or
	// http.public_url is https), "always" (behind a TLS-terminating proxy
	// without public_url) or "never" (plain-HTTP LAN setups).
	CookieSecure string `yaml:"cookie_secure" env:"SYNCR_AUTH_COOKIE_SECURE"`
	// SessionIdleTimeout ends a session unused this long (at least a
	// minute).
	SessionIdleTimeout time.Duration `yaml:"session_idle_timeout" env:"SYNCR_AUTH_SESSION_IDLE_TIMEOUT"`
	// SessionMaxAge ends a session this long after the login, however
	// much it is used (at least a minute).
	SessionMaxAge time.Duration `yaml:"session_max_age" env:"SYNCR_AUTH_SESSION_MAX_AGE"`
	// SetupCode presets the one-time code of the first-run setup (for
	// automation and tests); at least MinSetupCodeLen characters besides
	// dashes and spaces. When empty, the controller generates a random
	// code on every start until the first admin exists and logs it.
	SetupCode redact.Secret `yaml:"setup_code" env:"SYNCR_SETUP_CODE"`
}

// Cookie security modes of Auth.CookieSecure.
const (
	CookieSecureAuto   = "auto"
	CookieSecureAlways = "always"
	CookieSecureNever  = "never"
)

// MinSetupCodeLen is the shortest auth.setup_code, in characters besides
// dashes and white space (users.MinSetupCodeLen).
const MinSetupCodeLen = 16

// minSessionTimeout is the shortest session idle timeout and maximum age.
const minSessionTimeout = time.Minute

func (a Auth) validate() error {
	var errs []error
	switch a.CookieSecure {
	case CookieSecureAuto, CookieSecureAlways, CookieSecureNever:
	default:
		errs = append(errs, fmt.Errorf("auth.cookie_secure must be %s, %s or %s, not %q", CookieSecureAuto, CookieSecureAlways, CookieSecureNever, a.CookieSecure))
	}
	if a.SessionIdleTimeout < minSessionTimeout {
		errs = append(errs, fmt.Errorf("auth.session_idle_timeout must be at least %v", minSessionTimeout))
	}
	if a.SessionMaxAge < minSessionTimeout {
		errs = append(errs, fmt.Errorf("auth.session_max_age must be at least %v", minSessionTimeout))
	}
	if code := a.SetupCode.Reveal(); code != "" {
		n := 0
		for _, r := range code {
			switch {
			case r == '-' || unicode.IsSpace(r):
			case unicode.IsControl(r):
				errs = append(errs, errors.New("auth.setup_code must not contain control characters"))
				n = MinSetupCodeLen // reported once; do not also report the length
			default:
				n++
			}
		}
		if n < MinSetupCodeLen {
			errs = append(errs, fmt.Errorf("auth.setup_code must have at least %d characters besides dashes and spaces (it has %d)", MinSetupCodeLen, n))
		}
	}
	return errors.Join(errs...)
}

// Workers configures what the controller tells new workers and when it
// gives up on an unreachable one.
type Workers struct {
	// DownloadBaseURL is where the worker install and upgrade commands
	// download the syncr-worker release archives from: a mirror, which must
	// serve the release's file names (and checksums.txt). Empty, the
	// default, means the project's GitHub releases: the release of the
	// controller's own version when it is a release build, so new and
	// upgraded workers run the controller's version, and the latest release
	// (DefaultDownloadBaseURL) for a development build.
	DownloadBaseURL string `yaml:"download_base_url" env:"SYNCR_WORKERS_DOWNLOAD_BASE_URL"`
	// LostAfter ends the dispatched and running jobs of a worker that has
	// been out of contact this long (default 1h, at least MinLostAfter):
	// they become interrupted (worker_lost), or cancelled if a cancel was
	// requested. A worker that comes back later is told to stop them.
	LostAfter time.Duration `yaml:"lost_after" env:"SYNCR_WORKERS_LOST_AFTER"`
}

// MinLostAfter is the shortest workers.lost_after: well above the time a
// worker counts as offline (45 s) and its reconnect backoff (30 s).
const MinLostAfter = 5 * time.Minute

// DefaultDownloadBaseURL is where the commands of a development build
// download from when workers.download_base_url is empty: the latest
// release (equal to api.DefaultDownloadBaseURL; a test in
// cmd/syncr-controller keeps them in step). A release build's commands use
// its own release instead.
const DefaultDownloadBaseURL = "https://github.com/Zeptiny/Syncr/releases/latest/download"

func (w Workers) validate() error {
	var errs []error
	if w.DownloadBaseURL != "" {
		if err := validateHTTPURL(w.DownloadBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("workers.download_base_url: %w", err))
		}
	}
	if w.LostAfter < MinLostAfter {
		errs = append(errs, fmt.Errorf("workers.lost_after must be at least %v", MinLostAfter))
	}
	return errors.Join(errs...)
}

// validateHTTPURL accepts an absolute http or https URL with a host and
// without credentials, query or fragment.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a valid URL", raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("%q must be an absolute http:// or https:// URL", raw)
	case u.Host == "" || u.Opaque != "":
		return fmt.Errorf("%q has no host", raw)
	case u.User != nil:
		return errors.New("must not contain credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("%q must not have a query or fragment", raw)
	}
	return nil
}

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
		Auth: Auth{
			CookieSecure:       CookieSecureAuto,
			SessionIdleTimeout: 7 * 24 * time.Hour,
			SessionMaxAge:      30 * 24 * time.Hour,
		},
		Workers: Workers{LostAfter: time.Hour},
		Database: Database{
			MaxConns:       10,
			ConnectTimeout: 10 * time.Second,
		},
		Schedules: Schedules{Tick: 10 * time.Second},
		Jobs:      defaultJobs(),
		Notify:    defaultNotify(),
		Probe:     defaultProbe(),
	}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	return errors.Join(c.HTTP.validate(), c.Log.Validate(), c.Secrets.validate(), c.API.validate(),
		c.Auth.validate(), c.Workers.validate(), c.Database.validate(), c.Schedules.validate(), c.Jobs.validate(), c.Notify.validate(),
		c.Probe.validate(), c.Dev.validate())
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
	if h.PublicURL != "" {
		if err := validateHTTPURL(h.PublicURL); err != nil {
			errs = append(errs, fmt.Errorf("http.public_url: %w", err))
		}
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
			"it encrypts the credentials of remotes and workers at rest, so back it up"))
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
