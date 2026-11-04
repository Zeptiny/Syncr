package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseWorkerMode(t *testing.T) {
	t.Parallel()
	for _, m := range []WorkerMode{WorkerActive, WorkerPassive} {
		got, err := ParseWorkerMode(string(m))
		if err != nil || got != m {
			t.Errorf("ParseWorkerMode(%q) = (%q, %v)", m, got, err)
		}
	}
	for _, bad := range []string{"", "ACTIVE", "push"} {
		if _, err := ParseWorkerMode(bad); !errors.Is(err, ErrInvalidWorkerMode) {
			t.Errorf("ParseWorkerMode(%q) err = %v, want ErrInvalidWorkerMode", bad, err)
		}
	}
}

func TestDeriveWorkerStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	const threshold = 45 * time.Second
	tests := []struct {
		name     string
		lastSeen time.Time
		want     WorkerStatus
	}{
		{"never seen", time.Time{}, WorkerPending},
		{"just now", now, WorkerOnline},
		{"within threshold", now.Add(-threshold + time.Nanosecond), WorkerOnline},
		{"at threshold", now.Add(-threshold), WorkerOffline},
		{"long ago", now.Add(-time.Hour), WorkerOffline},
		{"future (clock skew)", now.Add(time.Minute), WorkerOnline},
	}
	for _, tt := range tests {
		if got := DeriveWorkerStatus(tt.lastSeen, now, threshold); got != tt.want {
			t.Errorf("%s: DeriveWorkerStatus = %q, want %q", tt.name, got, tt.want)
		}
	}
}
