package main

import (
	"testing"
	"time"

	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestProbeSettingsReachTheLink: probe.timeout and probe.restic_timeout are
// what the probe broker caps a probe at and, with the broker's grace, how
// long a passive worker's Probe may take to answer.
func TestProbeSettingsReachTheLink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		probe   controllerconfig.Probe
		wantCap time.Duration
	}{
		{"defaults", controllerconfig.Default().Probe, time.Minute},
		{"a longer restic timeout", controllerconfig.Probe{Timeout: 20 * time.Second, ResticTimeout: 75 * time.Second}, 75 * time.Second},
		{"a longer listing timeout", controllerconfig.Probe{Timeout: 40 * time.Second, ResticTimeout: 10 * time.Second}, 40 * time.Second},
		{"the longest allowed", controllerconfig.Probe{Timeout: link.MaxProbeTimeout, ResticTimeout: link.MaxProbeTimeout}, link.MaxProbeTimeout},
	}
	for _, tt := range tests {
		cfg := controllerconfig.Default()
		cfg.Probe = tt.probe
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		bc := probeConfig(cfg.Probe)
		if bc.MaxTimeout != tt.wantCap || bc.Wait() != tt.wantCap+link.ProbeGrace {
			t.Errorf("%s: broker cap %v, wait %v; want %v plus the grace", tt.name, bc.MaxTimeout, bc.Wait(), tt.wantCap)
		}
		if got := passiveClientConfig(cfg.Probe).ProbeTimeout; got != tt.wantCap+link.ProbeGrace {
			t.Errorf("%s: passive probe timeout %v, want %v", tt.name, got, tt.wantCap+link.ProbeGrace)
		}
	}
}
