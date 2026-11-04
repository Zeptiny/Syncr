package domain

import (
	"fmt"
	"time"
)

// WorkerMode selects which side of the worker link initiates calls. The
// string values are stable: they are stored and exposed through the API.
type WorkerMode string

// Worker modes.
const (
	// WorkerActive: the worker calls the controller (Report, Poll). Works
	// behind NAT and without open ports.
	WorkerActive WorkerMode = "active"
	// WorkerPassive: the controller calls the worker (Command, Pull) over TLS
	// with a pinned certificate.
	WorkerPassive WorkerMode = "passive"
)

// ParseWorkerMode converts a stored or transmitted value to a WorkerMode.
// Errors wrap ErrInvalidWorkerMode.
func ParseWorkerMode(s string) (WorkerMode, error) {
	m := WorkerMode(s)
	if !m.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidWorkerMode, s)
	}
	return m, nil
}

// Valid reports whether m is a known mode.
func (m WorkerMode) Valid() bool { return m == WorkerActive || m == WorkerPassive }

// WorkerStatus is a worker's connection status. It is derived from the time
// of the last contact (see DeriveWorkerStatus), never stored.
type WorkerStatus string

// Worker statuses.
const (
	// WorkerPending: enrolled or registered, never connected.
	WorkerPending WorkerStatus = "pending"
	// WorkerOnline: last contact within the offline threshold.
	WorkerOnline WorkerStatus = "online"
	// WorkerOffline: no contact within the offline threshold.
	WorkerOffline WorkerStatus = "offline"
)

// DeriveWorkerStatus returns the status of a worker last seen at lastSeen
// (zero if never), as of now. The worker is online while now - lastSeen <
// offlineAfter. A lastSeen in the future (clock adjustments) counts as
// online.
func DeriveWorkerStatus(lastSeen, now time.Time, offlineAfter time.Duration) WorkerStatus {
	switch {
	case lastSeen.IsZero():
		return WorkerPending
	case now.Sub(lastSeen) < offlineAfter:
		return WorkerOnline
	default:
		return WorkerOffline
	}
}
