package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/link"
)

// Probe configures the short operations the controller asks workers to run
// for the web UI and the repository use cases (internal/controller/probe):
// folder listings, remote tests and restic's snapshot listings.
type Probe struct {
	// Timeout bounds a folder listing and a remote test (default 30s).
	Timeout time.Duration `yaml:"timeout" env:"SYNCR_PROBE_TIMEOUT"`
	// ResticTimeout bounds the restic operations: creating or opening a
	// repository, listing its snapshots and browsing one (default 60s).
	// Raise it for large repositories on slow remotes.
	ResticTimeout time.Duration `yaml:"restic_timeout" env:"SYNCR_PROBE_RESTIC_TIMEOUT"`
}

// MinProbeTimeout is the shortest probe.timeout and probe.restic_timeout;
// the longest is link.MaxProbeTimeout, the cap both sides of the link
// enforce.
const MinProbeTimeout = time.Second

func defaultProbe() Probe {
	return Probe{Timeout: 30 * time.Second, ResticTimeout: 60 * time.Second}
}

// Longest returns the longer of the two timeouts: what an API call that
// runs a probe may take.
func (p Probe) Longest() time.Duration { return max(p.Timeout, p.ResticTimeout) }

func (p Probe) validate() error {
	var errs []error
	if p.Timeout < MinProbeTimeout || p.Timeout > link.MaxProbeTimeout {
		errs = append(errs, fmt.Errorf("probe.timeout must be between %v and %v", MinProbeTimeout, link.MaxProbeTimeout))
	}
	if p.ResticTimeout < MinProbeTimeout || p.ResticTimeout > link.MaxProbeTimeout {
		errs = append(errs, fmt.Errorf("probe.restic_timeout must be between %v and %v", MinProbeTimeout, link.MaxProbeTimeout))
	}
	return errors.Join(errs...)
}
