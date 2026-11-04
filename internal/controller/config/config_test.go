package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

func TestDefaultIsValid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"loopback addr", func(c *Config) { c.HTTP.Addr = "127.0.0.1:9000" }, ""},
		{"ephemeral port", func(c *Config) { c.HTTP.Addr = "localhost:0" }, ""},
		{"missing port", func(c *Config) { c.HTTP.Addr = "localhost" }, "http.addr"},
		{"bad port", func(c *Config) { c.HTTP.Addr = ":http-alt" }, "invalid port"},
		{"port out of range", func(c *Config) { c.HTTP.Addr = ":70000" }, "invalid port"},
		{"zero timeout", func(c *Config) { c.HTTP.ShutdownTimeout = 0 }, "shutdown_timeout"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	content := "http:\n  addr: \"127.0.0.1:9000\"\n  shutdown_timeout: 30s\nlog:\n  level: debug\n  format: json\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SYNCR_HTTP_ADDR": ":7000", "SYNCR_LOG_FORMAT": "text"}

	cfg := Default()
	err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":7000" || cfg.HTTP.ShutdownTimeout != 30*time.Second || cfg.HTTP.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("HTTP = %+v", cfg.HTTP)
	}
	if cfg.Log.Format != logging.FormatText || cfg.Log.Level.String() != "DEBUG" {
		t.Errorf("Log = %+v", cfg.Log)
	}
}
