package domain

import (
	"errors"
	"fmt"
	"slices"
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

func TestValidLabel(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"nas", "a", "0", "linux-x86_64", "eu.west.1", "a_b", "abcdefghijklmnopqrstuvwxyz012345"} {
		if !ValidLabel(ok) {
			t.Errorf("ValidLabel(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "NAS", "-a", ".a", "_a", "a b", "a/b", "é", "abcdefghijklmnopqrstuvwxyz0123456", "a\n"} {
		if ValidLabel(bad) {
			t.Errorf("ValidLabel(%q) = true", bad)
		}
	}
}

func TestNormalizeLabels(t *testing.T) {
	t.Parallel()
	got, err := NormalizeLabels([]string{"Linux", "nas", "NAS", "desktop"})
	if err != nil || !slices.Equal(got, []string{"desktop", "linux", "nas"}) {
		t.Errorf("NormalizeLabels = (%v, %v)", got, err)
	}
	if got, err := NormalizeLabels(nil); got != nil || err != nil {
		t.Errorf("NormalizeLabels(nil) = (%v, %v)", got, err)
	}
	_, err = NormalizeLabels([]string{"ok", "not ok", "-x"})
	var verr *ValidationError
	if !errors.As(err, &verr) || len(verr.Fields) != 2 || verr.Fields[0].Field != "labels[1]" || verr.Fields[1].Field != "labels[2]" {
		t.Errorf("invalid labels: %v", err)
	}
	many := make([]string, 0, MaxLabels+1)
	for i := range MaxLabels + 1 {
		many = append(many, fmt.Sprintf("l%d", i))
	}
	if _, err := NormalizeLabels(many); !errors.As(err, &verr) || verr.Fields[0].Field != "labels" {
		t.Errorf("too many labels: %v", err)
	}
	// Duplicates do not count against the limit.
	if _, err := NormalizeLabels(append(many[:MaxLabels:MaxLabels], "L0")); err != nil {
		t.Errorf("duplicate at the limit: %v", err)
	}
}
