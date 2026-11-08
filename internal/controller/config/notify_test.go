package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

func TestDefaultSchedulesAndNotify(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Schedules.Tick != 10*time.Second {
		t.Errorf("Schedules = %+v", cfg.Schedules)
	}
	want := Notify{NetworkPolicy: "private", MaxAttempts: 8, Concurrency: 4, SendTimeout: 15 * time.Second, Retention: 720 * time.Hour}
	if n := cfg.Notify; n.NetworkPolicy != want.NetworkPolicy || n.MaxAttempts != want.MaxAttempts || n.Concurrency != want.Concurrency ||
		n.SendTimeout != want.SendTimeout || n.Retention != want.Retention || len(n.AllowedNetworks) != 0 {
		t.Errorf("Notify = %+v, want %+v", n, want)
	}
	p, err := cfg.Notify.Policy()
	if err != nil || p.Mode != netguard.ModePrivate || len(p.Allow) != 0 {
		t.Errorf("Policy() = %+v, %v", p, err)
	}
}

func TestValidateSchedulesAndNotify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"tick 1s", func(c *Config) { c.Schedules.Tick = time.Second }, ""},
		{"tick too short", func(c *Config) { c.Schedules.Tick = 500 * time.Millisecond }, "schedules.tick"},
		{"tick too long", func(c *Config) { c.Schedules.Tick = time.Hour }, "schedules.tick"},
		{"policy public", func(c *Config) { c.Notify.NetworkPolicy = "public" }, ""},
		{"policy any", func(c *Config) { c.Notify.NetworkPolicy = "ANY" }, ""},
		{"policy empty", func(c *Config) { c.Notify.NetworkPolicy = "" }, ""},
		{"policy unknown", func(c *Config) { c.Notify.NetworkPolicy = "lan" }, "notify.network_policy"},
		{"allowed networks", func(c *Config) { c.Notify.AllowedNetworks = []string{"127.0.0.1", "10.0.0.0/8", "fd00::/8"} }, ""},
		{"allowed network bad", func(c *Config) { c.Notify.AllowedNetworks = []string{"10.0.0.0/33"} }, "notify.allowed_networks"},
		{"allowed network name", func(c *Config) { c.Notify.AllowedNetworks = []string{"localhost"} }, "notify.allowed_networks"},
		{"zero attempts", func(c *Config) { c.Notify.MaxAttempts = 0 }, "notify.max_attempts"},
		{"too many attempts", func(c *Config) { c.Notify.MaxAttempts = 51 }, "notify.max_attempts"},
		{"zero concurrency", func(c *Config) { c.Notify.Concurrency = 0 }, "notify.concurrency"},
		{"short send timeout", func(c *Config) { c.Notify.SendTimeout = 0 }, "notify.send_timeout"},
		{"long send timeout", func(c *Config) { c.Notify.SendTimeout = 2 * time.Minute }, "notify.send_timeout"},
		{"short retention", func(c *Config) { c.Notify.Retention = time.Minute }, "notify.retention"},
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

func TestLoadSchedulesAndNotify(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	content := "schedules:\n  tick: 30s\nnotify:\n  network_policy: public\n  allowed_networks: [\"192.168.1.10\"]\n" +
		"  max_attempts: 5\n  retention: 48h\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"SYNCR_SCHEDULES_TICK": "1s", "SYNCR_NOTIFY_NETWORK_POLICY": "any",
		"SYNCR_NOTIFY_ALLOWED_NETWORKS": "10.0.0.0/8,127.0.0.1", "SYNCR_NOTIFY_CONCURRENCY": "2",
		"SYNCR_NOTIFY_SEND_TIMEOUT": "5s",
	}
	cfg := Default()
	if err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Schedules.Tick != time.Second {
		t.Errorf("Schedules = %+v", cfg.Schedules)
	}
	n := cfg.Notify
	if n.NetworkPolicy != "any" || !slices.Equal(n.AllowedNetworks, []string{"10.0.0.0/8", "127.0.0.1"}) ||
		n.MaxAttempts != 5 || n.Concurrency != 2 || n.SendTimeout != 5*time.Second || n.Retention != 48*time.Hour {
		t.Errorf("Notify = %+v", n)
	}
	p, err := n.Policy()
	if err != nil || p.Mode != netguard.ModeAny || len(p.Allow) != 2 {
		t.Errorf("Policy() = %+v, %v", p, err)
	}
}

// TestLoadEmptyAllowedNetworks: an empty variable (as docker compose passes
// an unset one) allows nothing extra.
func TestLoadEmptyAllowedNetworks(t *testing.T) {
	t.Parallel()
	env := map[string]string{"SYNCR_NOTIFY_ALLOWED_NETWORKS": "", "SYNCR_NOTIFY_NETWORK_POLICY": "private"}
	cfg := Default()
	if err := platformconfig.Load("", func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.Notify.Policy(); err != nil || len(p.Allow) != 0 || p.Mode != netguard.ModePrivate {
		t.Errorf("Policy() = %+v, %v", p, err)
	}
}
