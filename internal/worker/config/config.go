// Package config defines the worker's configuration file and environment
// variables. Milestone 2 adds the link settings (mode, controller URL,
// listen address, credentials) and local_roots.
//
// Allowed dependencies: the standard library and internal/platform packages.
package config

import (
	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

// Config is the worker configuration.
type Config struct {
	Log logging.Config `yaml:"log"`
}

// Default returns the configuration used when nothing is overridden.
func Default() Config {
	return Config{Log: logging.DefaultConfig()}
}

// Validate reports every invalid setting.
func (c *Config) Validate() error {
	return c.Log.Validate()
}
