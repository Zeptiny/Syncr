package domain

import "errors"

// Domain errors. Match them with errors.Is; services wrap them with context
// (and sometimes join a more specific sentinel of their own). Adapters map
// them to transport codes:
//
//	error                                              Connect code
//	ErrNotFound                                        NotFound
//	ErrAlreadyExists                                   AlreadyExists
//	ErrInvalidArgument, ErrInvalidID,
//	ErrInvalidJobStatus, ErrInvalidWorkerMode           InvalidArgument
//	ErrFailedPrecondition, ErrInvalidTransition        FailedPrecondition
//	ErrConflict                                        Aborted
//	ErrUnauthenticated                                 Unauthenticated
//
// Anything else is an internal error.
var (
	// ErrInvalidID reports a malformed or nil entity ID.
	ErrInvalidID = errors.New("invalid id")
	// ErrInvalidJobStatus reports an unknown job status value.
	ErrInvalidJobStatus = errors.New("invalid job status")
	// ErrInvalidTransition reports a job status change the state machine
	// does not allow.
	ErrInvalidTransition = errors.New("invalid job status transition")
	// ErrInvalidWorkerMode reports an unknown worker mode value.
	ErrInvalidWorkerMode = errors.New("invalid worker mode")

	// ErrInvalidArgument reports a malformed request value.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNotFound reports a missing entity. Entities the caller may not see
	// (e.g. another worker's job) are reported as not found too.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists reports a create of an entity whose ID is taken.
	ErrAlreadyExists = errors.New("already exists")
	// ErrFailedPrecondition reports an operation the entity's current state
	// does not allow (e.g. a request from a worker process that has been
	// replaced).
	ErrFailedPrecondition = errors.New("failed precondition")
	// ErrConflict reports a lost race: a conditional update found the entity
	// changed since it was read. Repositories return it; services re-read
	// and retry, so callers rarely see it.
	ErrConflict = errors.New("concurrent modification")
	// ErrUnauthenticated reports missing, unknown or wrong credentials.
	ErrUnauthenticated = errors.New("unauthenticated")
)
