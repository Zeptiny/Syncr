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
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

// Config is the controller configuration.
type Config struct {
	HTTP HTTP           `yaml:"http"`
	Log  logging.Config `yaml:"log"`
}

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
		Log: logging.DefaultConfig(),
	}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	return errors.Join(c.HTTP.validate(), c.Log.Validate())
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
