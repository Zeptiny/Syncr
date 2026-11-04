package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
)

const apiTestToken = "0123456789abcdef0123456789abcdef"

func TestAPIValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"no token is allowed", func(*Config) {}, ""},
		{"32 char token", func(c *Config) { c.API.AdminToken = apiTestToken }, ""},
		{"short token", func(c *Config) { c.API.AdminToken = "short-but-secret" }, "at least 32 characters"},
		{"token with space", func(c *Config) { c.API.AdminToken = apiTestToken + " x" }, "without whitespace"},
		{"token with newline", func(c *Config) { c.API.AdminToken = apiTestToken + "\n" }, "without whitespace"},
		{"zero request timeout", func(c *Config) { c.API.RequestTimeout = 0 }, "request_timeout"},
		{"zero watch duration", func(c *Config) { c.API.WatchMaxDuration = 0 }, "watch_max_duration"},
		{"zero keepalive", func(c *Config) { c.API.WatchKeepalive = 0 }, "watch_keepalive"},
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
			case err != nil && strings.Contains(err.Error(), "short-but-secret"):
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestAPIFromEnvironmentIsRedacted(t *testing.T) {
	t.Parallel()
	env := map[string]string{"SYNCR_ADMIN_TOKEN": apiTestToken, "SYNCR_API_WATCH_MAX_DURATION": "10m"}
	cfg := Default()
	if err := platformconfig.Load("", func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.API.AdminToken.Reveal() != apiTestToken || cfg.API.WatchMaxDuration != 10*time.Minute {
		t.Errorf("API = %+v", cfg.API)
	}
	if got := fmt.Sprintf("%v %+v %#v", cfg.API.AdminToken, cfg.API, cfg); strings.Contains(got, apiTestToken) {
		t.Errorf("formatting leaks the token: %s", got)
	}
}
