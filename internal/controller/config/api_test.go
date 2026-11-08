package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
)

const removedToken = "0123456789abcdef0123456789abcdef-removed"

func TestAPIValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"defaults", func(*Config) {}, ""},
		{"removed admin token is accepted", func(c *Config) { c.API.AdminToken = true }, ""},
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
			}
		})
	}
}

// TestRemovedAdminToken: a configuration of Milestone 2 with the admin
// token still loads (from the environment or the file, even empty), the
// value is not kept anywhere, and RemovedSettings warns about it.
func TestRemovedAdminToken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name string
		path string
		env  map[string]string
		want bool
	}{
		{"unset", "", nil, false},
		{"environment", "", map[string]string{"SYNCR_ADMIN_TOKEN": removedToken}, true},
		{"empty environment variable", "", map[string]string{"SYNCR_ADMIN_TOKEN": ""}, true},
		{"file", write("token.yaml", "api:\n  admin_token: "+removedToken+"\n  watch_max_duration: 10m\n"), nil, true},
		{"file without the key", write("plain.yaml", "api:\n  watch_max_duration: 10m\n"), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			if err := platformconfig.Load(tt.path, func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok }, &cfg); err != nil {
				t.Fatal(err)
			}
			warnings := cfg.RemovedSettings()
			if got := len(warnings) == 1; got != tt.want {
				t.Fatalf("RemovedSettings() = %+v, want a warning: %v", warnings, tt.want)
			}
			if tt.want && (warnings[0].Key != "api.admin_token" || warnings[0].Env != "SYNCR_ADMIN_TOKEN" || !strings.Contains(warnings[0].Instead, "API token")) {
				t.Errorf("warning = %+v", warnings[0])
			}
			if tt.path != "" && strings.Contains(tt.path, "token") && cfg.API.WatchMaxDuration != 10*time.Minute {
				t.Errorf("the rest of the file was not applied: %+v", cfg.API)
			}
			if got := fmt.Sprintf("%v %+v %#v %+v", cfg, cfg, cfg, warnings); strings.Contains(got, removedToken) {
				t.Errorf("the removed token is kept: %s", got)
			}
		})
	}
}
