package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// fakeRunner is a test double; it also documents the minimal shape of a
// runner implementation.
type fakeRunner struct {
	kind string
}

func (f fakeRunner) Kind() string { return f.kind }

func (f fakeRunner) Run(_ context.Context, _ Job, emit EmitFunc) (Result, error) {
	emit(Progress{Bytes: 1, TotalBytes: 2})
	return Result{Stats: Progress{Bytes: 2, TotalBytes: 2}}, nil
}

func transferSpec() *syncrv1.JobSpec {
	return &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	if err := reg.Register(fakeRunner{kind: "rclone_transfer"}, fakeRunner{kind: "restic_backup"}); err != nil {
		t.Fatal(err)
	}

	if got := reg.Kinds(); !slices.Equal(got, []string{"rclone_transfer", "restic_backup"}) {
		t.Errorf("Kinds() = %q", got)
	}
	rn, err := reg.ForSpec(transferSpec())
	if err != nil || rn.Kind() != "rclone_transfer" {
		t.Fatalf("ForSpec() = (%v, %v)", rn, err)
	}

	var progress []Progress
	res, err := rn.Run(t.Context(), Job{ID: "j", Spec: transferSpec()}, func(p Progress) { progress = append(progress, p) })
	if err != nil || res.Stats.Bytes != 2 || len(progress) != 1 {
		t.Errorf("Run() = (%+v, %v), progress %v", res, err, progress)
	}
}

func TestRegistryForSpecErrors(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	if err := reg.Register(fakeRunner{kind: "restic_backup"}); err != nil {
		t.Fatal(err)
	}

	_, err := reg.ForSpec(transferSpec())
	if !errors.Is(err, ErrUnknownKind) || CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND {
		t.Errorf("unregistered kind: err = %v (code %v)", err, CodeOf(err))
	}
	for _, spec := range []*syncrv1.JobSpec{nil, {}} {
		_, err := reg.ForSpec(spec)
		if CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
			t.Errorf("ForSpec(%v): err = %v (code %v), want INVALID_SPEC", spec, err, CodeOf(err))
		}
	}
}

func TestRegistryRejectsBadRegistrations(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	if err := reg.Register(fakeRunner{kind: "restic_backup"}); err != nil {
		t.Fatal(err)
	}

	tests := map[string][]Runner{
		"nil":               {nil},
		"empty kind":        {fakeRunner{}},
		"unknown kind":      {fakeRunner{kind: "rsync"}},
		"already present":   {fakeRunner{kind: "restic_backup"}},
		"duplicate in call": {fakeRunner{kind: "restic_check"}, fakeRunner{kind: "restic_check"}},
		"atomic":            {fakeRunner{kind: "restic_prune"}, fakeRunner{kind: "nope"}},
	}
	for name, runners := range tests {
		if err := reg.Register(runners...); err == nil {
			t.Errorf("%s: Register() = nil, want error", name)
		}
	}
	if got := reg.Kinds(); !slices.Equal(got, []string{"restic_backup"}) {
		t.Errorf("failed registrations changed the registry: %q", got)
	}
}

func TestCodeOf(t *testing.T) {
	t.Parallel()
	pathErr := Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "path %q is outside local_roots", "/etc")
	tests := []struct {
		name string
		err  error
		want syncrv1.JobErrorCode
	}{
		{"nil", nil, syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED},
		{"classified", pathErr, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"wrapped classified", fmt.Errorf("prepare: %w", pathErr), syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"start error", &proc.StartError{Path: "rclone", Err: errors.New("not found")}, syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE},
		{"exit error", &proc.ExitError{Path: "rclone", ExitCode: 1}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		{"other", errors.New("boom"), syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL},
	}
	for _, tt := range tests {
		if got := CodeOf(tt.err); got != tt.want {
			t.Errorf("%s: CodeOf() = %v, want %v", tt.name, got, tt.want)
		}
	}
	if Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, nil) != nil {
		t.Error("Fail(code, nil) != nil")
	}
	if got := pathErr.Error(); got != `path "/etc" is outside local_roots` {
		t.Errorf("message = %q; the code must not leak into user-facing text", got)
	}
}

func TestCanceled(t *testing.T) {
	t.Parallel()
	if !Canceled(&proc.CanceledError{Cause: errors.New("custom cause")}) {
		t.Error("proc.CanceledError not recognized")
	}
	if !Canceled(fmt.Errorf("run: %w", context.Canceled)) {
		t.Error("context.Canceled not recognized")
	}
	if Canceled(errors.New("boom")) || Canceled(nil) {
		t.Error("plain error recognized as cancellation")
	}
}

func TestProgressProto(t *testing.T) {
	t.Parallel()
	p := Progress{
		Bytes: 3 << 40, TotalBytes: 4 << 40, // beyond 32 bits on purpose
		Files: 5, TotalFiles: 6, Checks: 7, Deletes: 8, Renames: 9, Errors: 10,
		Elapsed: 90 * time.Second,
	}
	want := &syncrv1.JobStats{
		Bytes: 3 << 40, TotalBytes: 4 << 40,
		Files: 5, TotalFiles: 6, Checks: 7, Deletes: 8, Renames: 9, Errors: 10,
		Elapsed: durationpb.New(90 * time.Second),
	}
	if got := p.Proto(); !proto.Equal(got, want) {
		t.Errorf("Proto() = %v, want %v", got, want)
	}
}
