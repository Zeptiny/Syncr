package jobkind_test

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

func withLimit(def *syncrv1.JobDefinition, d *durationpb.Duration) *syncrv1.JobDefinition {
	def.MaxRuntime = d
	return def
}

func TestMaxRuntimeCheck(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	const field = "definition.max_runtime"
	for _, tt := range []struct {
		name  string
		limit *durationpb.Duration
		ok    bool
	}{
		{"unset", nil, true},
		{"one minute", durationpb.New(time.Minute), true},
		{"six hours", durationpb.New(6 * time.Hour), true},
		{"366 days", durationpb.New(366 * 24 * time.Hour), true},
		{"zero is unset", durationpb.New(0), true},
		{"59 seconds", durationpb.New(59 * time.Second), false},
		{"negative", durationpb.New(-time.Hour), false},
		{"366 days and a second", durationpb.New(366*24*time.Hour + time.Second), false},
		{"invalid", &durationpb.Duration{Seconds: 3600, Nanos: -1}, false},
	} {
		_, _, err := r.Check(withLimit(transfer("", "/src"), tt.limit))
		switch {
		case tt.ok && err != nil:
			t.Errorf("%s: Check = %v", tt.name, err)
		case !tt.ok && !isValidation(err, field):
			t.Errorf("%s: Check = %v, want a field error on %s", tt.name, err, field)
		}
	}

	// Both the kind's errors and the limit's are reported.
	_, _, err = r.Check(withLimit(transfer("", ""), durationpb.New(time.Second)))
	if got := fields(err); !slices.Contains(got, field) || !slices.Contains(got, "definition.rclone_transfer.source.path") {
		t.Errorf("Check with two errors = %v", err)
	}
	// Every kind gets the check, restic ones included.
	backup := &syncrv1.JobDefinition{
		MaxRuntime: durationpb.New(time.Second),
		Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
			RepositoryId: domain.NewResticRepoID().String(), Paths: []string{"/srv"},
		}},
	}
	if _, _, err := r.Check(backup); !isValidation(err, field) {
		t.Errorf("restic backup with a 1s limit: %v", err)
	}
}

func TestMaxRuntimeDescribe(t *testing.T) {
	t.Parallel()
	r, err := jobkind.NewRegistry(echoKind{"rclone_transfer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		limit *durationpb.Duration
		want  string
	}{
		{nil, "echo /src"},
		{durationpb.New(6 * time.Hour), "echo /src · Time limit 6h"},
		{durationpb.New(90 * time.Minute), "echo /src · Time limit 1h30m"},
		{durationpb.New(45 * time.Minute), "echo /src · Time limit 45m"},
		{durationpb.New(2 * 24 * time.Hour), "echo /src · Time limit 2d"},
		{durationpb.New(36*time.Hour + 30*time.Second), "echo /src · Time limit 1d12h30s"},
		{durationpb.New(time.Minute + 1500*time.Millisecond), "echo /src · Time limit 1m1s"},
		{durationpb.New(0), "echo /src"},
		{&durationpb.Duration{Seconds: 1, Nanos: -1}, "echo /src"},
	} {
		if got := r.Describe(withLimit(transfer("", "/src"), tt.limit), jobkind.Names{}); got != tt.want {
			t.Errorf("Describe with %v = %q, want %q", tt.limit, got, tt.want)
		}
	}
}
