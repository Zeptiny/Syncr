package link

import "time"

// Time limits of probes, shared by both sides of the link so they agree:
// every other probe limit (the controller's settings, the probe broker,
// the passive client, the worker's executor, server and retention of
// probe state) is derived from these.
const (
	// MaxProbeTimeout caps the timeout of any probe (RunProbe.timeout). The
	// controller refuses longer probe settings and caps what it sends; a
	// worker caps what it receives.
	MaxProbeTimeout = 90 * time.Second
	// ProbeGrace is what the controller waits on top of a probe's timeout
	// for the worker's answer: its delivery and the way back.
	ProbeGrace = 5 * time.Second
)

// HeavyProbeOps returns the probe ops that walk a whole folder or
// repository (Milestone 7), in field-number order: measuring a worker's
// folder (measure_local), a remote's (measure_remote) or a snapshot's
// (restic_measure), searching snapshots (restic_find) and comparing two
// (restic_diff). Each holds one of the worker's heavy slots (one by
// default) as well as a normal probe slot; they end at a soft deadline
// before their timeout with what they found (complete=false), and the
// controller bounds them with its scan timeout instead of the listing or
// restic one.
func HeavyProbeOps() []string {
	return []string{"measure_local", "measure_remote", "restic_measure", "restic_find", "restic_diff"}
}

// IsHeavyProbeOp reports whether op is one of HeavyProbeOps.
func IsHeavyProbeOp(op string) bool {
	switch op {
	case "measure_local", "measure_remote", "restic_measure", "restic_find", "restic_diff":
		return true
	}
	return false
}
