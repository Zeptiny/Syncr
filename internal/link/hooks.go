package link

import (
	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Limits of job hooks (JobHooks, HookInfo, HookRun), shared by both sides of
// the link so they agree: the controller checks a definition against them,
// the worker its configuration and what it reports. They are the domain's.
const (
	// MaxHookNameLen bounds a hook name, in bytes.
	MaxHookNameLen = domain.MaxHookNameLen
	// MaxHooksPerPhase bounds each list of JobHooks.
	MaxHooksPerPhase = domain.MaxHooksPerPhase
	// MaxWorkerHooks bounds the hooks one worker defines (Hello.hooks).
	MaxWorkerHooks = domain.MaxWorkerHooks
	// MaxHookDescriptionLen bounds HookInfo.description, in characters.
	MaxHookDescriptionLen = domain.MaxHookDescriptionLen
	// MaxHookOutputBytes bounds HookRun.output.
	MaxHookOutputBytes = domain.MaxHookOutputBytes
	// MaxHookMessageBytes bounds HookRun.message.
	MaxHookMessageBytes = domain.MaxHookMessageBytes
)

// ValidHookName reports whether name is a hook name (domain.ValidHookName):
// 1 to MaxHookNameLen characters, each a lower-case ASCII letter, a digit,
// ".", "_" or "-", the first a letter or digit.
func ValidHookName(name string) bool { return domain.ValidHookName(name) }

// HookPhases returns the phases of a job's hooks in the order they run:
// before, on_success, on_failure, after.
func HookPhases() []syncrv1.HookPhase {
	return []syncrv1.HookPhase{
		syncrv1.HookPhase_HOOK_PHASE_BEFORE,
		syncrv1.HookPhase_HOOK_PHASE_ON_SUCCESS,
		syncrv1.HookPhase_HOOK_PHASE_ON_FAILURE,
		syncrv1.HookPhase_HOOK_PHASE_AFTER,
	}
}

// HookPhaseName returns the name of a phase: the JobHooks field that lists
// its hooks ("before", "on_success", "on_failure", "after": the domain's
// Hook* phases), which is also what a hook reads in SYNCR_HOOK_PHASE and
// what stores keep. Unknown and unspecified phases return "".
func HookPhaseName(p syncrv1.HookPhase) string {
	switch p {
	case syncrv1.HookPhase_HOOK_PHASE_BEFORE:
		return domain.HookBefore
	case syncrv1.HookPhase_HOOK_PHASE_ON_SUCCESS:
		return domain.HookOnSuccess
	case syncrv1.HookPhase_HOOK_PHASE_ON_FAILURE:
		return domain.HookOnFailure
	case syncrv1.HookPhase_HOOK_PHASE_AFTER:
		return domain.HookAfter
	}
	return ""
}

// ParseHookPhase is the inverse of HookPhaseName. Unknown names return
// HOOK_PHASE_UNSPECIFIED.
func ParseHookPhase(name string) syncrv1.HookPhase {
	for _, p := range HookPhases() {
		if HookPhaseName(p) == name {
			return p
		}
	}
	return syncrv1.HookPhase_HOOK_PHASE_UNSPECIFIED
}

// HookStatusName returns the name of a hook run's status ("succeeded",
// "failed", "timed_out", "cancelled"), as stores keep it. Unknown and
// unspecified statuses return "".
func HookStatusName(s syncrv1.HookStatus) string {
	switch s {
	case syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED:
		return domain.HookSucceeded
	case syncrv1.HookStatus_HOOK_STATUS_FAILED:
		return domain.HookFailed
	case syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT:
		return domain.HookTimedOut
	case syncrv1.HookStatus_HOOK_STATUS_CANCELLED:
		return domain.HookCancelled
	}
	return ""
}

// ParseHookStatus is the inverse of HookStatusName. Unknown names return
// HOOK_STATUS_UNSPECIFIED.
func ParseHookStatus(name string) syncrv1.HookStatus {
	for v := range syncrv1.HookStatus_name {
		if s := syncrv1.HookStatus(v); s != syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED && HookStatusName(s) == name {
			return s
		}
	}
	return syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED
}

// HooksOf returns the hooks h names for phase, in order: the JobHooks list
// of that name. Nil for a nil h and for an unknown phase.
func HooksOf(h *syncrv1.JobHooks, phase syncrv1.HookPhase) []string {
	switch phase {
	case syncrv1.HookPhase_HOOK_PHASE_BEFORE:
		return h.GetBefore()
	case syncrv1.HookPhase_HOOK_PHASE_ON_SUCCESS:
		return h.GetOnSuccess()
	case syncrv1.HookPhase_HOOK_PHASE_ON_FAILURE:
		return h.GetOnFailure()
	case syncrv1.HookPhase_HOOK_PHASE_AFTER:
		return h.GetAfter()
	}
	return nil
}

// HookNames returns every hook h names, once each, in the order of the
// phases and then of the lists. Nil when h names none.
func HookNames(h *syncrv1.JobHooks) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, p := range HookPhases() {
		for _, name := range HooksOf(h, p) {
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// HasHooks reports whether h names at least one hook.
func HasHooks(h *syncrv1.JobHooks) bool {
	for _, p := range HookPhases() {
		if len(HooksOf(h, p)) > 0 {
			return true
		}
	}
	return false
}
