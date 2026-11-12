// Package logging builds the structured slog logger shared by Syncr binaries.
//
// Every logger built here redacts secrets (see package redact). Libraries
// receive a *slog.Logger through their constructors; nothing in Syncr uses
// slog.Default or package-level loggers.
//
// Allowed dependencies: the standard library and internal/platform packages.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// Format selects the log encoding.
type Format string

// Supported formats.
const (
	// FormatText is logfmt-style key=value output, for humans and journald.
	FormatText Format = "text"
	// FormatJSON is one JSON object per line, for log collectors.
	FormatJSON Format = "json"
)

// ErrInvalidConfig is returned (wrapped) for an invalid Config.
var ErrInvalidConfig = errors.New("invalid log config")

// Config configures logging. It is embedded in each binary's configuration
// file under the "log" key.
type Config struct {
	// Level is the minimum level logged: debug, info, warn or error.
	Level slog.Level `yaml:"level" env:"SYNCR_LOG_LEVEL"`
	// Format is the output encoding: text or json.
	Format Format `yaml:"format" env:"SYNCR_LOG_FORMAT"`
}

// DefaultConfig returns info-level text logging.
func DefaultConfig() Config {
	return Config{Level: slog.LevelInfo, Format: FormatText}
}

// Validate reports whether the config is usable.
func (c Config) Validate() error {
	switch c.Format {
	case FormatText, FormatJSON:
		return nil
	default:
		return fmt.Errorf("%w: format %q (want %q or %q)", ErrInvalidConfig, c.Format, FormatText, FormatJSON)
	}
}

// New returns a logger writing to w according to cfg, with secret redaction.
func New(w io.Writer, cfg Config) (*slog.Logger, error) {
	h, err := NewHandler(w, cfg)
	if err != nil {
		return nil, err
	}
	return slog.New(h), nil
}

// NewHandler returns the slog.Handler behind New, for callers that wrap it.
func NewHandler(w io.Writer, cfg Config) (slog.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{
		Level:       cfg.Level,
		ReplaceAttr: redact.ReplaceAttr,
	}
	if cfg.Format == FormatJSON {
		return slog.NewJSONHandler(w, opts), nil
	}
	return slog.NewTextHandler(w, opts), nil
}

// Discard returns a logger that drops everything, for tests and optional
// dependencies.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
