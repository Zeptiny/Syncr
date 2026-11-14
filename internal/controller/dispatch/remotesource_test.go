package dispatch_test

import (
	"context"
	"slices"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// remoteBackupDef backs up the folder "photos" of the test remote with
// method into a repository (which the test env cannot resolve: a job that
// gets past the worker checks fails with repository_unavailable).
func remoteBackupDef(method syncrv1.ResticRemoteSourceMethod) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
		RepositoryId: domain.NewResticRepoID().String(),
		RemoteSource: &syncrv1.ResticRemoteSourceDefinition{RemoteId: testRemote().String(), Path: "photos", Method: method},
	}}}
}

// TestHelloRemoteSourcesAreStored: what a worker reports about reading the
// folders of remotes is stored with its info, reasons on one line; a
// worker that reports nothing has the zero value.
func TestHelloRemoteSourcesAreStored(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newProbeWorker(e.enroll("nas"))
	w.remoteSources = &syncrv1.RemoteSourceSupport{Mirror: true, MountReason: "/dev/fuse\nis missing"}
	w.restart()
	w.report()
	want := workers.RemoteSources{Mirror: true, MountReason: "/dev/fuse is missing"}
	if got := e.worker(w.id).Info.RemoteSources; got != want {
		t.Errorf("stored %+v, want %+v", got, want)
	}
	w.remoteSources = nil
	w.restart()
	w.report()
	if got := e.worker(w.id).Info.RemoteSources; got != (workers.RemoteSources{}) {
		t.Errorf("after a Hello without them: %+v", got)
	}
}

// TestRemoteSourceMethodAtDispatch: a backup of a remote goes only to a
// worker that can use its method; one that cannot fails the job with
// tool_unavailable and its reason, one without the feature with
// worker_outdated.
func TestRemoteSourceMethodAtDispatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	worker := func(name string, rs *syncrv1.RemoteSourceSupport, features []string) *fakeWorker {
		w := e.newProbeWorker(e.enroll(name))
		w.features, w.remoteSources = features, rs
		w.restart()
		w.report()
		return w
	}
	off := worker("off", &syncrv1.RemoteSourceSupport{MirrorReason: "remote_sources.mirror is off in worker.yaml", MountReason: "WinFsp is not installed"}, link.Features())
	nofuse := worker("nofuse", &syncrv1.RemoteSourceSupport{Mirror: true, MountReason: "/dev/fuse is missing"}, link.Features())
	silent := worker("silent", &syncrv1.RemoteSourceSupport{}, link.Features())
	old := worker("old", nil, slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureResticRemoteSource }))

	for _, tt := range []struct {
		name          string
		w             *fakeWorker
		method        syncrv1.ResticRemoteSourceMethod
		code, message string
	}{
		{"mirror off", off, syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR, jobs.CodeToolUnavailable,
			`worker "off" cannot keep a mirror of a remote: remote_sources.mirror is off in worker.yaml`},
		{"no mount", nofuse, syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT, jobs.CodeToolUnavailable,
			`worker "nofuse" cannot mount a remote: /dev/fuse is missing`},
		{"no reason", silent, syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT, jobs.CodeToolUnavailable,
			`worker "silent" cannot mount a remote: it gave no reason`},
		{"outdated", old, syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR, jobs.CodeWorkerOutdated,
			`worker "old" must be upgraded to run backups of a folder of a remote`},
		// Past the worker checks: the env has no repositories.
		{"mirror on a worker that can", nofuse, syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR, jobs.CodeRepositoryUnavailable, ""},
	} {
		j := e.runDef(tt.w.id, remoteBackupDef(tt.method))
		if got := started(tt.w.poll()); len(got) != 0 {
			t.Errorf("%s: the worker was sent %v", tt.name, got)
		}
		got := e.job(j.ID)
		if got.Status != domain.JobFailed || got.Error.Code != tt.code || (tt.message != "" && got.Error.Message != tt.message) {
			t.Errorf("%s: %s %q %q, want %s %q", tt.name, got.Status, got.Error.Code, got.Error.Message, tt.code, tt.message)
		}
	}
}

// TestProgressPhaseIsLive: the phase of a worker's progress sample reaches
// the job's watchers (one the controller knows; others are dropped), and
// is not stored.
func TestProgressPhaseIsLive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.newProbeWorker(e.enroll("nas"))
	w.report() // Hello
	j := e.run(w.id)
	if got := started(w.poll()); !slices.Equal(got, []string{j.ID.String()}) {
		t.Fatalf("sent %v", got)
	}
	w.start(j.ID.String())
	w.report()
	sub := e.rec.Subscribe(j.ID)
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, phase := range []string{link.ProgressPhaseMirror, "bogus", link.ProgressPhaseBackup} {
		e.clock.Advance(time.Minute)
		w.emit(&syncrv1.Event{Payload: &syncrv1.Event_Progress{Progress: &syncrv1.Progress{
			JobId: j.ID.String(), Phase: phase, Stats: &syncrv1.JobStats{Bytes: int64(i + 1)},
		}}})
		w.report()
		want := phase
		if phase == "bogus" {
			want = ""
		}
		for {
			u, err := sub.Next(ctx)
			if err != nil {
				t.Fatalf("waiting for the sample of phase %q: %v", phase, err)
			}
			if u.Progress == nil {
				continue
			}
			if u.Progress.Phase != want || u.Progress.Stats.Bytes != int64(i+1) {
				t.Errorf("live sample = %+v, want phase %q", *u.Progress, want)
			}
			break
		}
	}
	samples, err := e.jobs.Samples(context.Background(), testActor(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 {
		t.Fatal("no sample was stored")
	}
	for _, s := range samples {
		if s.Phase != "" {
			t.Errorf("stored sample with phase %q", s.Phase)
		}
	}
}
