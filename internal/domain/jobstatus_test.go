package domain

import (
	"errors"
	"testing"
)

func TestJobStatusTransitions(t *testing.T) {
	t.Parallel()
	// The full matrix: every pair not listed here must be rejected.
	allowed := map[[2]JobStatus]bool{
		{JobQueued, JobDispatched}: true,
		{JobQueued, JobFailed}:     true,
		{JobQueued, JobCancelled}:  true,

		{JobDispatched, JobRunning}:     true,
		{JobDispatched, JobSucceeded}:   true,
		{JobDispatched, JobFailed}:      true,
		{JobDispatched, JobCancelled}:   true,
		{JobDispatched, JobInterrupted}: true,

		{JobRunning, JobSucceeded}:   true,
		{JobRunning, JobFailed}:      true,
		{JobRunning, JobCancelled}:   true,
		{JobRunning, JobInterrupted}: true,
	}

	statuses := append(JobStatuses(), JobStatus("bogus"))
	for _, from := range statuses {
		for _, to := range statuses {
			want := allowed[[2]JobStatus{from, to}]
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s: CanTransitionTo = %v, want %v", from, to, got, want)
			}
			got, err := from.TransitionTo(to)
			switch {
			case want && (err != nil || got != to):
				t.Errorf("%s -> %s: TransitionTo = (%s, %v), want (%s, nil)", from, to, got, err, to)
			case !want:
				var te *TransitionError
				if !errors.Is(err, ErrInvalidTransition) || !errors.As(err, &te) || te.From != from || te.To != to {
					t.Errorf("%s -> %s: err = %v, want *TransitionError", from, to, err)
				}
				if got != from {
					t.Errorf("%s -> %s: rejected transition returned %s, want unchanged %s", from, to, got, from)
				}
			}
		}
	}
}

func TestJobStatusPredicates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status   JobStatus
		terminal bool
		active   bool
	}{
		{JobQueued, false, false},
		{JobDispatched, false, true},
		{JobRunning, false, true},
		{JobSucceeded, true, false},
		{JobFailed, true, false},
		{JobCancelled, true, false},
		{JobInterrupted, true, false},
	}
	if len(tests) != len(JobStatuses()) {
		t.Fatalf("table covers %d statuses, JobStatuses has %d", len(tests), len(JobStatuses()))
	}
	for _, tt := range tests {
		if got := tt.status.Terminal(); got != tt.terminal {
			t.Errorf("%s.Terminal() = %v, want %v", tt.status, got, tt.terminal)
		}
		if got := tt.status.Active(); got != tt.active {
			t.Errorf("%s.Active() = %v, want %v", tt.status, got, tt.active)
		}
		if tt.terminal && len(tt.status.AllowedTransitions()) != 0 {
			t.Errorf("terminal status %s has transitions", tt.status)
		}
		if !tt.terminal && len(tt.status.AllowedTransitions()) == 0 {
			t.Errorf("non-terminal status %s is a dead end", tt.status)
		}
	}
}

func TestParseJobStatus(t *testing.T) {
	t.Parallel()
	for _, s := range JobStatuses() {
		got, err := ParseJobStatus(string(s))
		if err != nil || got != s {
			t.Errorf("ParseJobStatus(%q) = (%q, %v)", s, got, err)
		}
	}
	for _, bad := range []string{"", "QUEUED", "done", "canceled"} {
		if _, err := ParseJobStatus(bad); !errors.Is(err, ErrInvalidJobStatus) {
			t.Errorf("ParseJobStatus(%q) err = %v, want ErrInvalidJobStatus", bad, err)
		}
	}
}
