package jobkind_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

func withHooks(def *syncrv1.JobDefinition, h *syncrv1.JobHooks) *syncrv1.JobDefinition {
	def.Hooks = h
	return def
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("hook-%d", i)
	}
	return out
}

func TestHooksCheck(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		hooks *syncrv1.JobHooks
		want  []string // the fields in error, in order; none means valid
	}{
		{name: "unset"},
		{name: "empty", hooks: &syncrv1.JobHooks{}},
		{name: "every phase", hooks: &syncrv1.JobHooks{Before: []string{"stop-app", "dump.db_1"}, OnSuccess: []string{"ping"}, OnFailure: []string{"alert"}, After: []string{"start-app"}}},
		{name: "one hook in several phases", hooks: &syncrv1.JobHooks{Before: []string{"notify"}, OnSuccess: []string{"notify"}, OnFailure: []string{"notify"}, After: []string{"notify"}}},
		{name: "eight in a phase", hooks: &syncrv1.JobHooks{After: names(link.MaxHooksPerPhase)}},
		{name: "nine in a phase", hooks: &syncrv1.JobHooks{OnSuccess: names(link.MaxHooksPerPhase + 1)}, want: []string{"definition.hooks.on_success"}},
		{name: "not a name", hooks: &syncrv1.JobHooks{Before: []string{"ok", "Not A Name"}}, want: []string{"definition.hooks.before[1]"}},
		{name: "empty name", hooks: &syncrv1.JobHooks{After: []string{""}}, want: []string{"definition.hooks.after[0]"}},
		{name: "a command instead of a name", hooks: &syncrv1.JobHooks{Before: []string{"rm -rf /"}}, want: []string{"definition.hooks.before[0]"}},
		{name: "a path instead of a name", hooks: &syncrv1.JobHooks{Before: []string{"/usr/bin/id"}}, want: []string{"definition.hooks.before[0]"}},
		{name: "too long", hooks: &syncrv1.JobHooks{OnFailure: []string{strings.Repeat("a", link.MaxHookNameLen+1)}}, want: []string{"definition.hooks.on_failure[0]"}},
		{name: "twice in a list", hooks: &syncrv1.JobHooks{Before: []string{"a", "b", "a"}}, want: []string{"definition.hooks.before[2]"}},
		{name: "several problems", hooks: &syncrv1.JobHooks{Before: []string{"A"}, OnSuccess: names(9), After: []string{"x", "x"}},
			want: []string{"definition.hooks.before[0]", "definition.hooks.on_success", "definition.hooks.after[1]"}},
	} {
		_, _, err := r.Check(withHooks(transfer("", "/src"), tt.hooks))
		if got := fields(err); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Check fields = %v (%v), want %v", tt.name, got, err, tt.want)
		}
	}

	// The kind's errors, the time limit's and the hooks' are all reported.
	def := withHooks(withLimit(transfer("", ""), durationpb.New(time.Second)), &syncrv1.JobHooks{Before: []string{"?"}})
	_, _, err = r.Check(def)
	got := fields(err)
	for _, want := range []string{"definition.rclone_transfer.source.path", "definition.max_runtime", "definition.hooks.before[0]"} {
		if !slices.Contains(got, want) {
			t.Errorf("Check with three errors = %v, want %s among them", err, want)
		}
	}

	// Every kind a user may start takes hooks, restores included.
	repo := domain.NewResticRepoID().String()
	hooks := &syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}
	for name, def := range map[string]*syncrv1.JobDefinition{
		"backup": {Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{RepositoryId: repo, Paths: []string{"/srv"}}}},
		"restore": {Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: &syncrv1.ResticRestoreDefinition{
			RepositoryId: repo, SnapshotId: "latest", Target: "/srv/restore"}}},
		"check": {Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{RepositoryId: repo}}},
	} {
		if _, _, err := r.Check(withHooks(def, hooks)); err != nil {
			t.Errorf("%s with hooks: Check = %v", name, err)
		}
		if _, _, err := r.Check(withHooks(def, &syncrv1.JobHooks{Before: []string{"no good"}})); !isValidation(err, "definition.hooks.before[0]") {
			t.Errorf("%s with an invalid hook name: Check = %v", name, err)
		}
	}

	// A kind only the system starts takes none: nobody chose them.
	del := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: repo}}}
	if _, _, err := r.Check(del); err != nil {
		t.Fatalf("restic_delete_data without hooks: Check = %v", err)
	}
	if _, _, err := r.Check(withHooks(del, hooks)); !isValidation(err, "definition.hooks") {
		t.Errorf("restic_delete_data with hooks: Check = %v, want a field error on definition.hooks", err)
	}
}

func TestBuildGivesTheSpecItsHooks(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(echoKind{"rclone_transfer"})
	if err != nil {
		t.Fatal(err)
	}
	hooks := &syncrv1.JobHooks{Before: []string{"stop-app"}, OnFailure: []string{"alert"}, After: []string{"start-app"}}
	def := withHooks(transfer("", "/src"), hooks)
	spec, err := r.Build(def, jobkind.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(spec.GetHooks(), hooks) {
		t.Errorf("spec hooks = %v, want %v", spec.GetHooks(), hooks)
	}
	// The spec owns its copy.
	spec.GetHooks().Before[0] = "changed"
	if def.GetHooks().GetBefore()[0] != "stop-app" {
		t.Error("Build shares the definition's hooks with the spec")
	}
	for name, h := range map[string]*syncrv1.JobHooks{"unset": nil, "empty": {}, "empty lists": {Before: []string{}}} {
		spec, err := r.Build(withHooks(transfer("", "/src"), h), jobkind.Resolved{})
		if err != nil || spec.GetHooks() != nil {
			t.Errorf("%s: spec hooks = %v, err = %v; want none", name, spec.GetHooks(), err)
		}
	}
	// Hooks are not part of what a job is called.
	if got := r.Describe(def, jobkind.Names{}); got != "echo /src" {
		t.Errorf("Describe = %q", got)
	}
}

func TestHookNamesAndMissingHooks(t *testing.T) {
	t.Parallel()
	def := withHooks(transfer("", "/src"), &syncrv1.JobHooks{
		Before: []string{"stop-app", "dump-db"}, OnSuccess: []string{"ping"}, OnFailure: []string{"alert", "ping"}, After: []string{"start-app", "stop-app"},
	})
	if got, want := jobkind.HookNames(def), []string{"stop-app", "dump-db", "ping", "alert", "start-app"}; !slices.Equal(got, want) {
		t.Errorf("HookNames = %v, want %v", got, want)
	}
	if got := jobkind.MissingHooks(def, []string{"start-app", "stop-app", "ping", "unused"}); !slices.Equal(got, []string{"dump-db", "alert"}) {
		t.Errorf("MissingHooks = %v, want dump-db and alert in the order they run", got)
	}
	if got := jobkind.MissingHooks(def, jobkind.HookNames(def)); got != nil {
		t.Errorf("MissingHooks with every hook defined = %v", got)
	}
	if got := jobkind.MissingHooks(def, nil); len(got) != 5 {
		t.Errorf("MissingHooks on a worker without hooks = %v", got)
	}
	none := transfer("", "/src")
	if jobkind.HookNames(none) != nil || jobkind.MissingHooks(none, nil) != nil || jobkind.HooksOf(none) != nil || jobkind.HooksOf(nil) != nil {
		t.Error("a definition without hooks names some")
	}

	for name, want := range map[string]string{
		"stop-app":  "definition.hooks.before[0]", // the first place it is named
		"dump-db":   "definition.hooks.before[1]",
		"ping":      "definition.hooks.on_success[0]",
		"alert":     "definition.hooks.on_failure[0]",
		"start-app": "definition.hooks.after[0]",
		"unnamed":   "definition.hooks",
	} {
		if got := jobkind.HookField(def, name); got != want {
			t.Errorf("HookField(%s) = %s, want %s", name, got, want)
		}
	}
}

// A stored definition keeps its hooks: jobs and schedules run with what the
// user saved.
func TestMarshalKeepsHooks(t *testing.T) {
	t.Parallel()
	def := withHooks(transfer("", "/src"), &syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}})
	b, err := jobkind.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"hooks":{"before":["stop-app"],"after":["start-app"]}`) {
		t.Errorf("stored definition = %s", b)
	}
	back, err := jobkind.Unmarshal(b)
	if err != nil || !proto.Equal(back, def) {
		t.Errorf("round trip = %v, %v; want %v", back, err, def)
	}
}
