package domain

import (
	"errors"
	"slices"
	"testing"
)

func TestParseNotificationEvent(t *testing.T) {
	t.Parallel()
	for _, e := range NotificationEvents() {
		got, err := ParseNotificationEvent(string(e))
		if err != nil || got != e {
			t.Errorf("ParseNotificationEvent(%q) = (%q, %v)", e, got, err)
		}
	}
	for _, bad := range []string{"", "FAILED", "cancelled", "queued"} {
		if _, err := ParseNotificationEvent(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ParseNotificationEvent(%q) err = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

func TestEventForJobStatus(t *testing.T) {
	t.Parallel()
	want := map[JobStatus]NotificationEvent{JobSucceeded: EventSucceeded, JobFailed: EventFailed, JobInterrupted: EventInterrupted}
	for _, st := range JobStatuses() {
		got, ok := EventForJobStatus(st)
		if w, notify := want[st]; ok != notify || got != w {
			t.Errorf("EventForJobStatus(%s) = (%q, %v), want (%q, %v)", st, got, ok, w, notify)
		}
	}
}

func TestEventSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		set   EventSet
		valid bool
		norm  EventSet
	}{
		{nil, true, nil},
		{EventSet{EventMissed, EventFailed}, true, EventSet{EventFailed, EventMissed}},
		{EventSet{EventFailed, EventFailed}, false, EventSet{EventFailed}},
		{EventSet{"bogus", EventSucceeded}, false, EventSet{EventSucceeded}},
	}
	for _, tt := range tests {
		if got := tt.set.Valid(); got != tt.valid {
			t.Errorf("%v.Valid() = %v", tt.set, got)
		}
		if got := tt.set.Normalize(); !slices.Equal(got, tt.norm) {
			t.Errorf("%v.Normalize() = %v, want %v", tt.set, got, tt.norm)
		}
	}
	s := EventSet{EventFailed, EventMissed}
	if !s.Contains(EventMissed) || s.Contains(EventSucceeded) {
		t.Error("Contains is wrong")
	}
	if s.Within(JobEvents()) || !s.Within(ScheduleEvents()) || !(EventSet{}).Within(nil) {
		t.Error("Within is wrong")
	}
	if slices.Contains(ScheduleEvents(), EventTest) || slices.Contains(JobEvents(), EventMissed) {
		t.Error("subscribable event lists contain events that cannot be subscribed")
	}
}
