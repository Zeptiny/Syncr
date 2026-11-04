package clock

import (
	"testing"
	"time"
)

func TestSystem(t *testing.T) {
	t.Parallel()
	before := time.Now().Add(-time.Second)
	got := System{}.Now()
	if got.Before(before) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("System.Now() = %v, not close to the real time", got)
	}
	if got.Location() != time.UTC {
		t.Errorf("System.Now() location = %v, want UTC", got.Location())
	}
	if got != got.Truncate(Precision) {
		t.Errorf("System.Now() = %v has sub-microsecond digits", got)
	}
	// No monotonic reading: Round(0) strips it, so equality must hold.
	if got != got.Round(0) {
		t.Error("System.Now() carries a monotonic clock reading")
	}
}

func TestFake(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 4, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	f := NewFake(start)
	want := time.Date(2026, 11, 4, 11, 0, 0, 123456000, time.UTC)
	if got := f.Now(); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("NewFake: Now() = %v, want %v", got, want)
	}
	if got := f.Advance(time.Minute); !got.Equal(want.Add(time.Minute)) {
		t.Errorf("Advance = %v", got)
	}
	f.Set(want)
	if got := f.Now(); !got.Equal(want) {
		t.Errorf("Set: Now() = %v", got)
	}
}
