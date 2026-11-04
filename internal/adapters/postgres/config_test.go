package postgres

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	valid := func() Config {
		c := DefaultConfig()
		c.URL = "postgres://u:p@localhost/db"
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"key/value dsn", func(c *Config) { c.URL = "host=localhost dbname=syncr" }, ""},
		{"no url", func(c *Config) { c.URL = "  " }, "url is required"},
		{"zero max conns", func(c *Config) { c.MaxConns = 0 }, "max conns"},
		{"min above max", func(c *Config) { c.MinConns = c.MaxConns + 1 }, "min conns"},
		{"negative min", func(c *Config) { c.MinConns = -1 }, "min conns"},
		{"negative lifetime", func(c *Config) { c.MaxConnLifetime = -time.Second }, "max conn lifetime"},
		{"negative idle", func(c *Config) { c.MaxConnIdleTime = -time.Second }, "max conn idle"},
		{"zero health check", func(c *Config) { c.HealthCheckPeriod = 0 }, "health check"},
		{"zero connect timeout", func(c *Config) { c.ConnectTimeout = 0 }, "connect timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigNeverPrintsURL(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.URL = "postgres://user:hunter2@localhost/db"
	for _, out := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("config formatting leaks the password: %s", out)
		}
	}
}

// TestOpenErrorsHideThePassword: connection strings carry the password, and
// Open's errors end up in logs.
func TestOpenErrorsHideThePassword(t *testing.T) {
	t.Parallel()
	for name, url := range map[string]string{
		"unparsable":  "postgres://user:hunter2@localhost:notaport/db",
		"unreachable": "postgres://user:hunter2@127.0.0.1:1/db?sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.URL = redact.Secret(url)
			cfg.ConnectTimeout = 2 * time.Second
			s, err := Open(t.Context(), cfg)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestOpenRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	if _, err := Open(t.Context(), Config{}); err == nil {
		t.Fatal("Open(zero Config) succeeded")
	}
}
