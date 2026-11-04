package proc

import (
	"fmt"
	"path/filepath"
)

// StartError reports that a child process could not be started: the
// executable is missing or not executable, the Spec is invalid, or the OS
// refused to create the process.
type StartError struct {
	Path string
	Err  error
}

// Error implements error.
func (e *StartError) Error() string {
	return fmt.Sprintf("proc: start %s: %v", filepath.Base(e.Path), e.Err)
}

// Unwrap returns the underlying error.
func (e *StartError) Unwrap() error { return e.Err }

// ExitError reports that a child exited unsuccessfully on its own (not
// because Run's context was done).
type ExitError struct {
	Path string
	// ExitCode is the exit code, or -1 if the child was terminated by a
	// signal.
	ExitCode int
	// Err is the underlying *exec.ExitError.
	Err error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.ExitCode < 0 {
		return fmt.Sprintf("proc: %s terminated: %v", filepath.Base(e.Path), e.Err)
	}
	return fmt.Sprintf("proc: %s exited with code %d", filepath.Base(e.Path), e.ExitCode)
}

// Unwrap returns the underlying error.
func (e *ExitError) Unwrap() error { return e.Err }

// CanceledError reports that Run's context was done before the child exited
// on its own. The child was stopped (gracefully if it exited within the grace
// period, otherwise killed).
type CanceledError struct {
	Path string
	// ExitCode is the code the child exited with after being stopped, or -1
	// if it was killed by a signal or never started.
	ExitCode int
	// Cause is context.Cause of Run's context, e.g. context.Canceled.
	Cause error
}

// Error implements error.
func (e *CanceledError) Error() string {
	return fmt.Sprintf("proc: %s stopped: %v", filepath.Base(e.Path), e.Cause)
}

// Unwrap returns the cancellation cause, so errors.Is(err, context.Canceled)
// works.
func (e *CanceledError) Unwrap() error { return e.Cause }
