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
