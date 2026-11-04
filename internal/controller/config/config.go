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
	"strconv"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

// Config is the controller configuration.
type Config struct {
	HTTP    HTTP           `yaml:"http"`
	Log     logging.Config `yaml:"log"`
	Secrets Secrets        `yaml:"secrets"`
}

// Secrets configures the encryption of secrets at rest (job specs, worker
// secrets) with AES-256-GCM (internal/adapters/secrets). Keys are 32 random
// bytes in standard base64, e.g. from `openssl rand -base64 32`. Losing the
// key makes every stored secret unreadable.
//
// To rotate: move the current key to previous_keys as "<key_id>:<key>", set
// a new key with a new key_id, restart; stored values are re-encrypted with
// the new key as they are rewritten, after which the old key can go.
type Secrets struct {
	// Key is the active master key. Optional until the controller persists
	// secrets; then required.
	Key string `yaml:"key" env:"SYNCR_SECRETS_KEY"`
	// KeyID identifies Key inside ciphertexts (default 1).
	KeyID uint32 `yaml:"key_id" env:"SYNCR_SECRETS_KEY_ID"`
	// PreviousKeys are "<id>:<base64 key>" entries still accepted for
	// decryption (comma-separated in the environment).
	PreviousKeys []string `yaml:"previous_keys" env:"SYNCR_SECRETS_PREVIOUS_KEYS"`
}

// secretsKeySize is the AES-256 key size.
const secretsKeySize = 32

// HTTP configures the API/UI listener.
type HTTP struct {
	// Addr is the listen address, e.g. ":8080" or "127.0.0.1:8080".
	Addr string `yaml:"addr" env:"SYNCR_HTTP_ADDR"`
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout" env:"SYNCR_HTTP_READ_HEADER_TIMEOUT"`
	// ShutdownTimeout bounds graceful shutdown: in-flight requests get this
	// long to finish after SIGINT/SIGTERM.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" env:"SYNCR_HTTP_SHUTDOWN_TIMEOUT"`
}

// Default returns the configuration used when nothing is overridden.
func Default() Config {
	return Config{
		HTTP: HTTP{
			Addr:              ":8080",
			ReadHeaderTimeout: 10 * time.Second,
			ShutdownTimeout:   15 * time.Second,
		},
		Log:     logging.DefaultConfig(),
		Secrets: Secrets{KeyID: 1},
	}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	return errors.Join(c.HTTP.validate(), c.Log.Validate(), c.Secrets.validate())
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
	return errors.Join(errs...)
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
