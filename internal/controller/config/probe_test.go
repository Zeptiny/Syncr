package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/link"
	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
)

func TestDefaultProbe(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Probe.Timeout != 30*time.Second || cfg.Probe.ResticTimeout != time.Minute {
		t.Errorf("Probe = %+v", cfg.Probe)
	}
	if got := cfg.Probe.Longest(); got != time.Minute {
		t.Errorf("Longest() = %v", got)
	}
}

func TestValidateProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"shortest", func(c *Config) { c.Probe = Probe{Timeout: time.Second, ResticTimeout: time.Second} }, ""},
		{"longest", func(c *Config) { c.Probe = Probe{Timeout: link.MaxProbeTimeout, ResticTimeout: link.MaxProbeTimeout} }, ""},
		{"timeout too short", func(c *Config) { c.Probe.Timeout = 999 * time.Millisecond }, "probe.timeout"},
		{"timeout too long", func(c *Config) { c.Probe.Timeout = link.MaxProbeTimeout + time.Second }, "probe.timeout"},
		{"restic timeout zero", func(c *Config) { c.Probe.ResticTimeout = 0 }, "probe.restic_timeout"},
		{"restic timeout too long", func(c *Config) { c.Probe.ResticTimeout = link.MaxProbeTimeout + time.Second }, "probe.restic_timeout"},
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

func TestLoadProbe(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	if err := os.WriteFile(path, []byte("probe:\n  timeout: 10s\n  restic_timeout: 45s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SYNCR_PROBE_RESTIC_TIMEOUT": "90s"}
	cfg := Default()
	if err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Probe.Timeout != 10*time.Second || cfg.Probe.ResticTimeout != 90*time.Second {
		t.Errorf("Probe = %+v", cfg.Probe)
	}
}
