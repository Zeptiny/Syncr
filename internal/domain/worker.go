package domain

import (
	"fmt"
	"slices"
	"strings"
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

// Limits of worker labels.
const (
	// MaxLabels bounds the labels of one worker.
	MaxLabels = 16
	// MaxLabelLen bounds one label, in bytes.
	MaxLabelLen = 32
)

// ValidLabel reports whether label is a valid, normalized worker label:
// ^[a-z0-9][a-z0-9_.-]{0,31}$. Labels are set by admins and targeted by
// schedules (any worker with the label).
func ValidLabel(label string) bool {
	if label == "" || len(label) > MaxLabelLen {
		return false
	}
	for i := range len(label) {
		c := label[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case i > 0 && (c == '_' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// NormalizeLabels lower-cases labels, drops duplicates and sorts them. It
// returns a *ValidationError (field "labels[i]" for an invalid label, at
// the index of the input, or "labels" for too many) if a label is invalid
// after lower-casing or there are more than MaxLabels distinct ones. The
// result is nil for no labels.
func NormalizeLabels(labels []string) ([]string, error) {
	var v Validation
	var out []string
	for i, l := range labels {
		l = strings.ToLower(l)
		if !ValidLabel(l) {
			v.Add(fmt.Sprintf("labels[%d]", i), "must be 1 to 32 characters of a-z, 0-9, \".\", \"_\" and \"-\", starting with a letter or digit")
			continue
		}
		out = append(out, l)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxLabels {
		v.Add("labels", fmt.Sprintf("at most %d labels are allowed", MaxLabels))
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
