package domain

import (
	"fmt"
	"slices"
)

// NotificationEvent is what a notification reports. The string values are
// stable: they are stored (contacts, schedule links, deliveries) and sent in
// notification payloads.
type NotificationEvent string

// Notification events.
const (
	// EventSucceeded: a job finished successfully.
	EventSucceeded NotificationEvent = "succeeded"
	// EventFailed: a job failed (a scheduled job that was never dispatched
	// included), or a scheduled run could not be launched.
	EventFailed NotificationEvent = "failed"
	// EventInterrupted: a job was interrupted (worker restart, shutdown or
	// removal).
	EventInterrupted NotificationEvent = "interrupted"
	// EventMissed: a scheduled run did not start in time.
	EventMissed NotificationEvent = "missed"
	// EventTest: a test notification. It is never stored or subscribed to.
	EventTest NotificationEvent = "test"
)

// NotificationEvents returns every event, in display order.
func NotificationEvents() []NotificationEvent {
	return []NotificationEvent{EventSucceeded, EventFailed, EventInterrupted, EventMissed, EventTest}
}

// JobEvents returns the events of finished jobs, which a contact may
// subscribe to for its owner's manual jobs (contacts' job_events).
func JobEvents() []NotificationEvent {
	return []NotificationEvent{EventSucceeded, EventFailed, EventInterrupted}
}

// ScheduleEvents returns the events a schedule's contact link may subscribe
// to: the job events plus missed runs.
func ScheduleEvents() []NotificationEvent {
	return []NotificationEvent{EventSucceeded, EventFailed, EventInterrupted, EventMissed}
}

// ParseNotificationEvent converts a stored or transmitted value to a
// NotificationEvent. Errors wrap ErrInvalidArgument.
func ParseNotificationEvent(s string) (NotificationEvent, error) {
	e := NotificationEvent(s)
	if !e.Valid() {
		return "", fmt.Errorf("%w: notification event %q", ErrInvalidArgument, s)
	}
	return e, nil
}

// Valid reports whether e is a known event.
func (e NotificationEvent) Valid() bool { return slices.Contains(NotificationEvents(), e) }

// EventForJobStatus returns the event a job that reached status raises:
// succeeded, failed and interrupted jobs notify; cancelled jobs and
// unfinished statuses never do (ok is false).
func EventForJobStatus(status JobStatus) (e NotificationEvent, ok bool) {
	switch status {
	case JobSucceeded:
		return EventSucceeded, true
	case JobFailed:
		return EventFailed, true
	case JobInterrupted:
		return EventInterrupted, true
	default:
		return "", false
	}
}

// EventSet is a set of notification events, e.g. the events a contact link
// subscribes to. Normalized sets are sorted in NotificationEvents order
// without duplicates; the zero value is the empty set.
type EventSet []NotificationEvent

// Valid reports whether every event of s is known and none repeats.
func (s EventSet) Valid() bool {
	for i, e := range s {
		if !e.Valid() || slices.Contains(s[:i], e) {
			return false
		}
	}
	return true
}

// Contains reports whether e is in s.
func (s EventSet) Contains(e NotificationEvent) bool { return slices.Contains(s, e) }

// Within reports whether every event of s is one of allowed (e.g.
// JobEvents or ScheduleEvents).
func (s EventSet) Within(allowed []NotificationEvent) bool {
	for _, e := range s {
		if !slices.Contains(allowed, e) {
			return false
		}
	}
	return true
}

// Normalize returns the known events of s in NotificationEvents order,
// without duplicates; nil if there are none. Unknown events are dropped, so
// check Valid first where they are an error.
func (s EventSet) Normalize() EventSet {
	var out EventSet
	for _, e := range NotificationEvents() {
		if s.Contains(e) {
			out = append(out, e)
		}
	}
	return out
}
