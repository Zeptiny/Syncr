package dispatch_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// limited returns def with max_runtime d (nil: unset).
func limited(def *syncrv1.JobDefinition, d *durationpb.Duration) *syncrv1.JobDefinition {
	def.MaxRuntime = d
	return def
}

// storedJob is a job of testOwner with def, as the jobs service stores it.
func storedJob(t *testing.T, def *syncrv1.JobDefinition) jobs.Job {
	t.Helper()
	raw, err := jobkind.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{ID: domain.NewJobID(), OwnerID: testOwner, Kind: jobkind.KindOf(def), Definition: raw}
}

// TestSpecsTimeLimit: the spec's max_runtime is the definition's, else the
// configured default, never the default for a kind only the system starts;
// a worker without the time-limit feature gets no job with a limit
// (ErrTimeLimitUnsupported, which is ErrWorkerOutdated), but still every
// job without one.
func TestSpecsTimeLimit(t *testing.T) {
	t.Parallel()
	repoID := domain.NewResticRepoID()
	repos := &fakeRepos{repo: restic.Resolved{
		ID: repoID, OwnerID: testOwner, RemoteID: testRemote(), Path: "vault", Password: testRepoPassword, ResticID: strings.Repeat("ab", 32),
	}}
	deleteData := func() *syncrv1.JobDefinition {
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{
			ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: repoID.String()},
		}}
	}
	current := workers.Info{Features: link.Features()}
	old := workers.Info{Features: []string{link.FeatureProbe, link.FeatureResticV2, link.FeatureResticDelete}}

	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		// dflt is the configured default.
		dflt time.Duration
		want time.Duration
	}{
		{"no limit", testDefinition("/data"), 0, 0},
		{"the definition's", limited(testDefinition("/data"), durationpb.New(6*time.Hour)), 0, 6 * time.Hour},
		{"the definition's over the default", limited(testDefinition("/data"), durationpb.New(time.Hour)), 24 * time.Hour, time.Hour},
		{"the default", testDefinition("/data"), 24 * time.Hour, 24 * time.Hour},
		{"an explicit zero is unset", limited(testDefinition("/data"), durationpb.New(0)), 2 * time.Hour, 2 * time.Hour},
		{"a system kind never gets the default", deleteData(), 24 * time.Hour, 0},
		{"a system kind keeps its own", limited(deleteData(), durationpb.New(3*time.Hour)), 24 * time.Hour, 3 * time.Hour},
	}
	for _, tt := range tests {
		specs, err := dispatch.NewSpecs(newFakeRemotes(), repos, kinds(t), dispatch.SpecsConfig{DefaultMaxRuntime: tt.dflt})
		if err != nil {
			t.Fatal(err)
		}
		j := storedJob(t, tt.def)
		if got := specs.TimeLimit(tt.def); got != tt.want {
			t.Errorf("%s: TimeLimit = %v, want %v", tt.name, got, tt.want)
		}
		if got := specs.TimeLimitOf(j); got != tt.want {
			t.Errorf("%s: TimeLimitOf = %v, want %v", tt.name, got, tt.want)
		}
		spec, err := specs.Build(context.Background(), j, current)
		if err != nil {
			t.Errorf("%s: Build: %v", tt.name, err)
			continue
		}
		switch got := spec.GetMaxRuntime(); {
		case tt.want == 0 && got != nil:
			t.Errorf("%s: spec max_runtime = %v, want none", tt.name, got.AsDuration())
		case tt.want > 0 && got.AsDuration() != tt.want:
			t.Errorf("%s: spec max_runtime = %v, want %v", tt.name, got.AsDuration(), tt.want)
		}

		// A worker that cannot enforce a limit runs the job only without one.
		spec, err = specs.Build(context.Background(), j, old)
		switch {
		case tt.want == 0 && (err != nil || spec.GetMaxRuntime() != nil):
			t.Errorf("%s: on an old worker = (%v, %v), want the job without a limit", tt.name, spec.GetMaxRuntime(), err)
		case tt.want > 0 && (!errors.Is(err, dispatch.ErrTimeLimitUnsupported) || !errors.Is(err, dispatch.ErrWorkerOutdated) ||
			!errors.Is(err, domain.ErrFailedPrecondition)):
			t.Errorf("%s: on an old worker err = %v, want ErrTimeLimitUnsupported", tt.name, err)
		}
	}

	// A kind's own missing feature is reported as that, not as the limit.
	specs, err := dispatch.NewSpecs(newFakeRemotes(), repos, kinds(t), dispatch.SpecsConfig{DefaultMaxRuntime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := specs.Build(context.Background(), storedJob(t, deleteData()), workers.Info{}); !errors.Is(err, dispatch.ErrWorkerOutdated) ||
		errors.Is(err, dispatch.ErrTimeLimitUnsupported) {
		t.Errorf("a pre-M5 worker: err = %v, want ErrWorkerOutdated for the kind", err)
	}
	// An undecodable stored definition has no limit; an invalid duration
	// (which cannot be stored) counts as unset.
	if got := specs.TimeLimitOf(jobs.Job{Definition: []byte("{")}); got != 0 {
		t.Errorf("TimeLimitOf(garbage) = %v", got)
	}
	if got := specs.TimeLimit(limited(testDefinition("/data"), &durationpb.Duration{Seconds: 1, Nanos: -1})); got != time.Hour {
		t.Errorf("TimeLimit(invalid duration) = %v, want the default", got)
	}
}

func TestNewSpecsChecksTheDefault(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{-time.Minute, time.Second, 59 * time.Second, jobs.MaxTimeLimit + time.Second} {
		if _, err := dispatch.NewSpecs(nil, nil, kinds(t), dispatch.SpecsConfig{DefaultMaxRuntime: d}); err == nil {
			t.Errorf("a default time limit of %v was accepted", d)
		}
	}
	for _, d := range []time.Duration{0, jobs.MinTimeLimit, 6 * time.Hour, jobs.MaxTimeLimit} {
		if _, err := dispatch.NewSpecs(nil, nil, kinds(t), dispatch.SpecsConfig{DefaultMaxRuntime: d}); err != nil {
			t.Errorf("a default time limit of %v: %v", d, err)
		}
	}
	if _, err := dispatch.NewSpecs(nil, nil, kinds(t), dispatch.SpecsConfig{}, dispatch.SpecsConfig{}); err == nil {
		t.Error("two configurations were accepted")
	}
	if jobs.MinTimeLimit != time.Minute || jobs.MaxTimeLimit != 366*24*time.Hour {
		t.Errorf("bounds = %v, %v", jobs.MinTimeLimit, jobs.MaxTimeLimit)
	}
}

// timeLimitEnv is a dispatcher whose specs have a default time limit of
// two hours.
func timeLimitEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	specs, err := dispatch.NewSpecs(e.remotes, nil, kinds(t), dispatch.SpecsConfig{DefaultMaxRuntime: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if e.disp, err = dispatch.New(dispatch.Deps{Jobs: e.jobs, Workers: e.workers, Specs: specs, Clock: e.clock, Hub: e.hub, Logger: slog.New(slog.NewTextHandler(e.logs, nil))}, e.cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestTimeLimitNeedsTheFeature: a job with a time limit on a worker that
// cannot enforce one fails with worker_outdated and says why; a current
// worker is sent the limit in its spec.
func TestTimeLimitNeedsTheFeature(t *testing.T) {
	t.Parallel()
	e := timeLimitEnv(t)
	old, current := e.newFakeWorker(e.enroll("old")), e.newProbeWorker(e.enroll("current"))
	old.report()
	current.report()

	outdated := e.run(old.id)
	if got := started(old.poll()); len(got) != 0 {
		t.Errorf("the old worker was sent %v", got)
	}
	if got := e.job(outdated.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeWorkerOutdated ||
		got.Error.Message != `worker "old" must be upgraded to run jobs with a time limit` {
		t.Errorf("outdated = %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}

	fine := e.runDef(current.id, limited(testDefinition("/data"), durationpb.New(45*time.Minute)))
	if got := started(current.poll()); !slices.Equal(got, []string{fine.ID.String()}) {
		t.Fatalf("the current worker was sent %v, want %v", got, fine.ID)
	}
	if got := current.specs[fine.ID.String()].GetMaxRuntime(); got.AsDuration() != 45*time.Minute {
		t.Errorf("spec max_runtime = %v, want 45m", got)
	}
}

// TestTimeLimitResultFailsTheJob: a job its worker stopped at its time
// limit ends failed with time_limit and notifies like any failure, also
// when a worker reports it with another status than FAILED.
func TestTimeLimitResultFailsTheJob(t *testing.T) {
	t.Parallel()
	e := timeLimitEnv(t)
	w := e.newProbeWorker(e.enroll("nas"))
	w.report()
	limitErr := &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT, Message: "stopped after 2h0m0s: the job's time limit was reached"}
	for _, st := range []syncrv1.JobResultStatus{
		syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED,
		syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED,
		syncrv1.JobResultStatus_JOB_RESULT_STATUS_UNSPECIFIED,
	} {
		before := e.rec.failures()
		j := e.run(w.id)
		w.poll()
		w.start(j.ID.String())
		w.report()
		w.finish(j.ID.String(), st, limitErr)
		w.report()
		got := e.job(j.ID)
		if got.Status != domain.JobFailed || got.Error.Code != jobs.CodeTimeLimit || got.Error.Message != limitErr.GetMessage() {
			t.Errorf("%s: job = %s %q %q, want failed time_limit", st, got.Status, got.Error.Code, got.Error.Message)
		}
		if e.rec.failures() != before+1 {
			t.Errorf("%s: the failure was not notified", st)
		}
	}
}
