package runner

import (
	"context"
	"errors"
	"fmt"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// Error attaches a failure category to a runner error. Return it (possibly
// wrapped) from Runner.Run to control the JobErrorCode the controller sees.
type Error struct {
	Code syncrv1.JobErrorCode
	Err  error
}

// Error implements error. The message is the underlying error's, without
// the code, because it is shown to users as is.
func (e *Error) Error() string { return e.Err.Error() }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Fail wraps err with code. It returns nil if err is nil.
func Fail(code syncrv1.JobErrorCode, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}

// Errorf formats an error (supporting %w) and wraps it with code.
func Errorf(code syncrv1.JobErrorCode, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeOf classifies a non-nil runner error:
//   - the code of the first *Error in the chain, if any;
//   - TOOL_UNAVAILABLE for *proc.StartError (missing or unusable binary);
//   - EXECUTION_FAILED for *proc.ExitError (the tool reported failure);
//   - INTERNAL otherwise.
//
// Cancellation is not a failure code: callers check for it first (see
// Runner.Run). CodeOf returns UNSPECIFIED for a nil error.
func CodeOf(err error) syncrv1.JobErrorCode {
	if err == nil {
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	if _, ok := errors.AsType[*proc.StartError](err); ok {
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE
	}
	if _, ok := errors.AsType[*proc.ExitError](err); ok {
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
	}
	return syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL
}

// Canceled reports whether err ended a run because its context was done:
// a *proc.CanceledError or a context error anywhere in the chain.
func Canceled(err error) bool {
	if _, ok := errors.AsType[*proc.CanceledError](err); ok {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
