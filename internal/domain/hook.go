package domain

// A hook is a command the operator of a worker defined in the worker's
// configuration under a name. Jobs name hooks to have them run around their
// operation; the controller never sees more of a hook than its name, a
// description and a timeout (docs/hooks.md).

// Limits of hooks. The worker applies them to its configuration, the
// controller to job definitions and to what workers report.
const (
	// MaxHookNameLen bounds a hook name, in bytes.
	MaxHookNameLen = 63
	// MaxHooksPerPhase bounds the hooks a job names for one phase (before,
	// on_success, on_failure, after).
	MaxHooksPerPhase = 8
	// MaxWorkerHooks bounds the hooks one worker defines.
	MaxWorkerHooks = 64
	// MaxHookDescriptionLen bounds a hook's description, in characters.
	MaxHookDescriptionLen = 200
	// MaxHookOutputBytes bounds the output kept of one run of a hook.
	MaxHookOutputBytes = 4096
	// MaxHookMessageBytes bounds the message of one run of a hook.
	MaxHookMessageBytes = 500
)

// ValidHookName reports whether name is a hook name: 1 to MaxHookNameLen
// characters, each a lower-case ASCII letter, a digit, ".", "_" or "-", the
// first a letter or digit. Names are compared as they are, so there is one
// spelling of each, and none needs quoting anywhere it is shown or passed.
func ValidHookName(name string) bool {
	if name == "" || len(name) > MaxHookNameLen {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// The phases of a job's hooks, in the order they run. They are the names of
// the lists in a job definition (and of the link's JobHooks fields).
const (
	// HookBefore: before the operation; a failure skips the operation.
	HookBefore = "before"
	// HookOnSuccess: after the operation succeeded.
	HookOnSuccess = "on_success"
	// HookOnFailure: after the job failed.
	HookOnFailure = "on_failure"
	// HookAfter: last, whatever happened.
	HookAfter = "after"
)

// ValidHookPhase reports whether phase is one of the Hook* phases.
func ValidHookPhase(phase string) bool {
	switch phase {
	case HookBefore, HookOnSuccess, HookOnFailure, HookAfter:
		return true
	}
	return false
}

// How one run of a hook ended.
const (
	// HookSucceeded: the command exited with code 0.
	HookSucceeded = "succeeded"
	// HookFailed: it exited with another code or could not be started.
	HookFailed = "failed"
	// HookTimedOut: the worker stopped it at the hook's timeout.
	HookTimedOut = "timed_out"
	// HookCancelled: the worker stopped it with its job (a cancel, the
	// job's time limit, a shutdown).
	HookCancelled = "cancelled"
)

// ValidHookStatus reports whether status is one of the Hook* run statuses.
func ValidHookStatus(status string) bool {
	switch status {
	case HookSucceeded, HookFailed, HookTimedOut, HookCancelled:
		return true
	}
	return false
}
