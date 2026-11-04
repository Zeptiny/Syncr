package domain

import (
	"fmt"
	"slices"
)

// JobStatus is the lifecycle state of a job. The string values are stable:
// they are stored in the database and exposed through the API.
//
// Lifecycle:
//
//	queued ──► dispatched ──► running ──► succeeded | failed | cancelled | interrupted
//	   │            │
//	   │            └──► succeeded | failed | cancelled | interrupted
//	   └──► failed | cancelled
//
// A dispatched job may finish without ever being observed running: the
// worker's result can be the first event the controller ingests. Terminal
// statuses never change; a retry is a new job.
type JobStatus string

// Job statuses.
const (
	// JobQueued: created, waiting to be delivered to a worker.
	JobQueued JobStatus = "queued"
	// JobDispatched: the worker acknowledged the StartJob command.
	JobDispatched JobStatus = "dispatched"
	// JobRunning: the worker reported the job's process started.
	JobRunning JobStatus = "running"
	// JobSucceeded: the worker reported success. Terminal.
	JobSucceeded JobStatus = "succeeded"
	// JobFailed: the job could not be dispatched or the worker reported
	// failure. Terminal.
	JobFailed JobStatus = "failed"
	// JobCancelled: stopped on request before completing. Terminal.
	JobCancelled JobStatus = "cancelled"
	// JobInterrupted: the worker lost the job (restart, crash) as detected by
	// reconcile. Terminal.
	JobInterrupted JobStatus = "interrupted"
)

// JobStatuses returns every job status in lifecycle order.
func JobStatuses() []JobStatus {
	return []JobStatus{JobQueued, JobDispatched, JobRunning, JobSucceeded, JobFailed, JobCancelled, JobInterrupted}
}

// ParseJobStatus converts a stored or transmitted value to a JobStatus.
// Errors wrap ErrInvalidJobStatus.
func ParseJobStatus(s string) (JobStatus, error) {
	st := JobStatus(s)
	if !st.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidJobStatus, s)
	}
	return st, nil
}

// Valid reports whether s is a known status.
func (s JobStatus) Valid() bool {
	return slices.Contains(JobStatuses(), s)
}

// Terminal reports whether s is a final status that never changes.
func (s JobStatus) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled, JobInterrupted:
		return true
	}
	return false
}

// Active reports whether a job in status s may currently occupy a worker
// (dispatched or running). Reconcile compares active jobs with the worker's
// own state.
func (s JobStatus) Active() bool {
	return s == JobDispatched || s == JobRunning
}

// AllowedTransitions returns the statuses s may move to. It is the single
// source of truth for the job state machine; terminal and unknown statuses
// return nil. A "transition" to the same status is not a transition: callers
// ingesting idempotent events should treat it as a no-op before asking.
func (s JobStatus) AllowedTransitions() []JobStatus {
	switch s {
	case JobQueued:
		return []JobStatus{JobDispatched, JobFailed, JobCancelled}
	case JobDispatched:
		return []JobStatus{JobRunning, JobSucceeded, JobFailed, JobCancelled, JobInterrupted}
	case JobRunning:
		return []JobStatus{JobSucceeded, JobFailed, JobCancelled, JobInterrupted}
	}
	return nil
}

// CanTransitionTo reports whether the state machine allows s -> next.
func (s JobStatus) CanTransitionTo(next JobStatus) bool {
	return slices.Contains(s.AllowedTransitions(), next)
}

// TransitionTo returns next if s -> next is allowed, or a *TransitionError
// (matching ErrInvalidTransition) otherwise.
func (s JobStatus) TransitionTo(next JobStatus) (JobStatus, error) {
	if !s.CanTransitionTo(next) {
		return s, &TransitionError{From: s, To: next}
	}
	return next, nil
}

// TransitionError reports a disallowed job status change.
type TransitionError struct {
	From, To JobStatus
}

// Error implements error.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("%s: %q -> %q", ErrInvalidTransition, e.From, e.To)
}

// Unwrap makes errors.Is(err, ErrInvalidTransition) true.
func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }
