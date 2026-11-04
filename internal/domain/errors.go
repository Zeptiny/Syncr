package domain

import "errors"

// Domain errors. Adapters map them to transport codes (e.g. Connect's
// InvalidArgument or FailedPrecondition); match them with errors.Is.
var (
	// ErrInvalidID reports a malformed or nil entity ID.
	ErrInvalidID = errors.New("invalid id")
	// ErrInvalidJobStatus reports an unknown job status value.
	ErrInvalidJobStatus = errors.New("invalid job status")
	// ErrInvalidTransition reports a job status change the state machine
	// does not allow.
	ErrInvalidTransition = errors.New("invalid job status transition")
)
