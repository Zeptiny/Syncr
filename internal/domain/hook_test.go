package domain

import (
	"strings"
	"testing"
)

func TestValidHookName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "0", "dump-postgres", "stop.app_2", "a-", strings.Repeat("a", MaxHookNameLen)} {
		if !ValidHookName(name) {
			t.Errorf("ValidHookName(%q) = false", name)
		}
	}
	for _, name := range []string{
		"", "-a", ".a", "_a", "Dump", "dump postgres", "dump/postgres", "dump\npostgres", "dümp", "a;b", "$(a)", "a\x00",
		strings.Repeat("a", MaxHookNameLen+1),
	} {
		if ValidHookName(name) {
			t.Errorf("ValidHookName(%q) = true", name)
		}
	}
}

func TestHookPhasesAndStatuses(t *testing.T) {
	t.Parallel()
	for _, p := range []string{HookBefore, HookOnSuccess, HookOnFailure, HookAfter} {
		if !ValidHookPhase(p) {
			t.Errorf("ValidHookPhase(%q) = false", p)
		}
	}
	for _, s := range []string{HookSucceeded, HookFailed, HookTimedOut, HookCancelled} {
		if !ValidHookStatus(s) {
			t.Errorf("ValidHookStatus(%q) = false", s)
		}
	}
	for _, bad := range []string{"", "Before", "pre", "finally", "ok", "running"} {
		if ValidHookPhase(bad) || ValidHookStatus(bad) {
			t.Errorf("%q is accepted as a phase or status", bad)
		}
	}
}
