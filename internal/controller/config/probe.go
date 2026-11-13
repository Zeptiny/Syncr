package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/link"
)

// Probe configures the short operations the controller asks workers to run
// for the web UI and the repository use cases (internal/controller/probe):
// folder listings, remote tests, restic's snapshot listings, and measuring
// folders, searching and comparing snapshots.
type Probe struct {
	// Timeout bounds a folder listing and a remote test (default 30s).
	Timeout time.Duration `yaml:"timeout" env:"SYNCR_PROBE_TIMEOUT"`
	// ResticTimeout bounds the restic operations: creating or opening a
	// repository, listing its snapshots and browsing one (default 60s).
	// Raise it for large repositories on slow remotes.
	ResticTimeout time.Duration `yaml:"restic_timeout" env:"SYNCR_PROBE_RESTIC_TIMEOUT"`
	// ScanTimeout bounds the scans (Milestone 7): measuring a folder's
	// size, searching a repository's snapshots and comparing two snapshots
	// (default 45s). A scan that reaches it answers with what it found so
	// far. A reverse proxy in front of the controller must allow it plus
	// 15s.
	ScanTimeout time.Duration `yaml:"scan_timeout" env:"SYNCR_PROBE_SCAN_TIMEOUT"`
}

// MinProbeTimeout is the shortest probe.timeout, probe.restic_timeout and
// probe.scan_timeout; the longest is link.MaxProbeTimeout, the cap both
// sides of the link enforce.
const MinProbeTimeout = time.Second

func defaultProbe() Probe {
	return Probe{Timeout: 30 * time.Second, ResticTimeout: 60 * time.Second, ScanTimeout: 45 * time.Second}
}

// Longest returns the longest of the timeouts: what an API call that runs
// a probe may take.
func (p Probe) Longest() time.Duration { return max(p.Timeout, p.ResticTimeout, p.ScanTimeout) }

func (p Probe) validate() error {
	var errs []error
	if p.Timeout < MinProbeTimeout || p.Timeout > link.MaxProbeTimeout {
		errs = append(errs, fmt.Errorf("probe.timeout must be between %v and %v", MinProbeTimeout, link.MaxProbeTimeout))
	}
	if p.ResticTimeout < MinProbeTimeout || p.ResticTimeout > link.MaxProbeTimeout {
		errs = append(errs, fmt.Errorf("probe.restic_timeout must be between %v and %v", MinProbeTimeout, link.MaxProbeTimeout))
	}
	if p.ScanTimeout < MinProbeTimeout || p.ScanTimeout > link.MaxProbeTimeout {
		errs = append(errs, fmt.Errorf("probe.scan_timeout must be between %v and %v", MinProbeTimeout, link.MaxProbeTimeout))
	}
	return errors.Join(errs...)
}
