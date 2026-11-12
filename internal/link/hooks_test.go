package link

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func protoName(s string) protoreflect.Name { return protoreflect.Name(s) }

func TestValidHookName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "0", "dump-postgres", "stop.app_2", "a-", strings.Repeat("a", MaxHookNameLen)} {
		if !ValidHookName(name) {
			t.Errorf("ValidHookName(%q) = false", name)
		}
	}
	for _, name := range []string{
		"", "-a", ".a", "_a", "Dump", "dump postgres", "dump/postgres", "dump\npostgres", "dümp", "a;b", "$(a)",
		strings.Repeat("a", MaxHookNameLen+1),
	} {
		if ValidHookName(name) {
			t.Errorf("ValidHookName(%q) = true", name)
		}
	}
}

func TestHookPhases(t *testing.T) {
	t.Parallel()
	want := []string{"before", "on_success", "on_failure", "after"}
	var got []string
	for _, p := range HookPhases() {
		name := HookPhaseName(p)
		got = append(got, name)
		if ParseHookPhase(name) != p {
			t.Errorf("ParseHookPhase(%q) = %v, want %v", name, ParseHookPhase(name), p)
		}
		if !domain.ValidHookPhase(name) {
			t.Errorf("phase %q is not a domain phase", name)
		}
		// The names are the JobHooks field names: the wire contract.
		if (&syncrv1.JobHooks{}).ProtoReflect().Descriptor().Fields().ByName(protoName(name)) == nil {
			t.Errorf("phase %q is not a JobHooks field", name)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("phase names = %q, want %q", got, want)
	}
	if n := (&syncrv1.JobHooks{}).ProtoReflect().Descriptor().Fields().Len(); n != len(want) {
		t.Errorf("JobHooks has %d fields, HookPhases covers %d", n, len(want))
	}
	if len(syncrv1.HookPhase_name) != len(want)+1 {
		t.Errorf("HookPhase has %d values, HookPhases covers %d", len(syncrv1.HookPhase_name)-1, len(want))
	}
	for _, name := range []string{"", "pre", "Before", "finally"} {
		if p := ParseHookPhase(name); p != syncrv1.HookPhase_HOOK_PHASE_UNSPECIFIED {
			t.Errorf("ParseHookPhase(%q) = %v", name, p)
		}
	}
	if HookPhaseName(syncrv1.HookPhase_HOOK_PHASE_UNSPECIFIED) != "" || HookPhaseName(99) != "" {
		t.Error("an unknown phase has a name")
	}
}

func TestHookStatusNames(t *testing.T) {
	t.Parallel()
	want := map[syncrv1.HookStatus]string{
		syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED: "succeeded",
		syncrv1.HookStatus_HOOK_STATUS_FAILED:    "failed",
		syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT: "timed_out",
		syncrv1.HookStatus_HOOK_STATUS_CANCELLED: "cancelled",
	}
	for v := range syncrv1.HookStatus_name {
		s := syncrv1.HookStatus(v)
		if s == syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED {
			continue
		}
		name, ok := want[s]
		if !ok {
			t.Errorf("status %v has no expected name: extend HookStatusName", s)
			continue
		}
		if got := HookStatusName(s); got != name {
			t.Errorf("HookStatusName(%v) = %q, want %q", s, got, name)
		}
		if got := ParseHookStatus(name); got != s {
			t.Errorf("ParseHookStatus(%q) = %v, want %v", name, got, s)
		}
		if !domain.ValidHookStatus(name) {
			t.Errorf("status %q is not a domain status", name)
		}
	}
	if HookStatusName(syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED) != "" || HookStatusName(99) != "" {
		t.Error("an unknown status has a name")
	}
	if ParseHookStatus("") != syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED || ParseHookStatus("ok") != syncrv1.HookStatus_HOOK_STATUS_UNSPECIFIED {
		t.Error("an unknown name parses to a status")
	}
}

func TestHookNames(t *testing.T) {
	t.Parallel()
	h := &syncrv1.JobHooks{
		Before:    []string{"stop-app", "dump-db"},
		OnSuccess: []string{"ping"},
		OnFailure: []string{"alert", "ping"},
		After:     []string{"start-app", "stop-app"},
	}
	if got, want := HookNames(h), []string{"stop-app", "dump-db", "ping", "alert", "start-app"}; !slices.Equal(got, want) {
		t.Errorf("HookNames = %q, want %q", got, want)
	}
	if !HasHooks(h) || !HasHooks(&syncrv1.JobHooks{After: []string{"x"}}) {
		t.Error("HasHooks = false for hooks")
	}
	if HasHooks(nil) || HasHooks(&syncrv1.JobHooks{}) || HookNames(nil) != nil || HookNames(&syncrv1.JobHooks{}) != nil {
		t.Error("an empty JobHooks names hooks")
	}
	if got := HooksOf(h, syncrv1.HookPhase_HOOK_PHASE_ON_FAILURE); !slices.Equal(got, []string{"alert", "ping"}) {
		t.Errorf("HooksOf(on_failure) = %q", got)
	}
	if HooksOf(h, syncrv1.HookPhase_HOOK_PHASE_UNSPECIFIED) != nil || HooksOf(nil, syncrv1.HookPhase_HOOK_PHASE_AFTER) != nil {
		t.Error("HooksOf returned hooks for no phase or no hooks")
	}
}
