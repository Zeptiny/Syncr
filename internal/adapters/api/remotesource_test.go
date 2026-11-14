package api

import (
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

const (
	apiMirror = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR
	apiMount  = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT
)

// remoteBackupDef backs up path on remote into repository repoID.
func remoteBackupDef(repoID string, remote domain.RemoteID, path string, method syncrv1.ResticRemoteSourceMethod) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
		RepositoryId: repoID,
		RemoteSource: &syncrv1.ResticRemoteSourceDefinition{RemoteId: remote.String(), Path: path, Method: method},
	}}}
}

// setWorkerSources makes the env's worker report features and what it can
// do with the folders of remotes.
func (e *testEnv) setWorkerSources(features []string, rs workers.RemoteSources) {
	e.t.Helper()
	info := workers.Info{Version: "v2.2.0", OS: "linux", Features: features, RemoteSources: rs, ReportedAt: e.clock.Now()}
	if err := e.store.SetWorkerInfo(e.t.Context(), e.worker, "", info); err != nil {
		e.t.Fatal(err)
	}
}

// TestRunJobRemoteSource: RunJob checks a backup of a remote against its
// repository and the caller's remotes, and refuses a worker known not to
// use its method, with the worker's reason.
func TestRunJobRemoteSource(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	repo := e.createRepo("vault") // at restic/vault on e.remote
	photos := e.addRemote(testUser, "Photos", "s3")
	disk := e.addRemote(testUser, "NAS disk", "local")
	theirs := e.addRemote(testOther, "Theirs", "s3")
	const srcField = "definition.restic_backup.remote_source."

	// Before the worker's first report nothing is known about it.
	j, err := e.runAs(testUser, remoteBackupDef(repo.GetId(), photos, "Pictures", apiMirror))
	if err != nil {
		t.Fatalf("RunJob = %v", err)
	}
	if j.GetTitle() != "Back up Photos/Pictures (mirror) → vault" || !slices.ContainsFunc(j.GetRemotes(), func(r *syncrv1.JobRemoteRef) bool {
		return r.GetId() == photos.String()
	}) {
		t.Errorf("job = %v", j)
	}
	stored, err := e.jsvc.Get(t.Context(), testUser, e.jobID(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ConcurrencyKeys) != 2 || !slices.ContainsFunc(stored.ConcurrencyKeys, func(k string) bool {
		return len(k) > len(jobkind.RemoteSourceKeyPrefix) && k[:len(jobkind.RemoteSourceKeyPrefix)] == jobkind.RemoteSourceKeyPrefix
	}) {
		t.Errorf("concurrency keys = %q", stored.ConcurrencyKeys)
	}

	for _, tt := range []struct {
		name, field, message string
		def                  *syncrv1.JobDefinition
	}{
		{"the repository's folder", srcField + "path",
			`is the folder of repository "vault" (at "restic/vault"): a backup cannot read its own repository`,
			remoteBackupDef(repo.GetId(), e.remote, "/restic/vault/", apiMirror)},
		{"a folder holding it", srcField + "path",
			`holds repository "vault" (at "restic/vault"): a backup cannot read its own repository; choose a folder beside it`,
			remoteBackupDef(repo.GetId(), e.remote, "", apiMirror)},
		{"a folder inside it", srcField + "path",
			`is inside the folder of repository "vault" (at "restic/vault"): a backup cannot read its own repository`,
			remoteBackupDef(repo.GetId(), e.remote, "restic/vault/data", apiMount)},
		{"a local remote", srcField + "remote_id", restic.SourceLocalMessage, remoteBackupDef(repo.GetId(), disk, "x", apiMirror)},
		{"no method", srcField + "method", "must be mirror or mount", remoteBackupDef(repo.GetId(), photos, "x", 0)},
	} {
		_, err := e.runAs(testUser, tt.def)
		if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, tt.field) != tt.message {
			t.Errorf("%s: RunJob = %v (%v), want %s: %s", tt.name, err, violations(t, err), tt.field, tt.message)
		}
	}
	// A folder beside the repository on its remote is fine.
	if _, err := e.runAs(testUser, remoteBackupDef(repo.GetId(), e.remote, "restic/vault2", apiMirror)); err != nil {
		t.Errorf("beside the repository: %v", err)
	}
	// Another user's remote and repository are not found.
	if _, err := e.runAs(testUser, remoteBackupDef(repo.GetId(), theirs, "x", apiMirror)); codeOf(t, err) != connect.CodeNotFound {
		t.Errorf("another user's remote: %v", err)
	}
	if _, err := e.runAs(testOther, remoteBackupDef(repo.GetId(), theirs, "x", apiMirror)); codeOf(t, err) != connect.CodeNotFound {
		t.Errorf("another user's repository: %v", err)
	}

	// The worker reports what it can do.
	e.setWorkerSources(link.Features(), workers.RemoteSources{Mirror: true, MountReason: "/dev/fuse is missing"})
	if _, err := e.runAs(testUser, remoteBackupDef(repo.GetId(), photos, "Pictures", apiMirror)); err != nil {
		t.Errorf("mirror on a worker that can: %v", err)
	}
	_, err = e.runAs(testUser, remoteBackupDef(repo.GetId(), photos, "Pictures", apiMount))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "worker_id") != `worker "worker" cannot mount a remote: /dev/fuse is missing` {
		t.Errorf("mount on a worker that cannot = %v (%v)", err, violations(t, err))
	}
	e.setWorkerSources(link.Features(), workers.RemoteSources{MirrorReason: "remote_sources.mirror is off in worker.yaml"})
	_, err = e.runAs(testUser, remoteBackupDef(repo.GetId(), photos, "Pictures", apiMirror))
	if codeOf(t, err) != connect.CodeInvalidArgument ||
		violation(err, "worker_id") != `worker "worker" cannot keep a mirror of a remote: remote_sources.mirror is off in worker.yaml` {
		t.Errorf("mirror on a worker that cannot = %v (%v)", err, violations(t, err))
	}
	old := slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureResticRemoteSource })
	e.setWorkerSources(old, workers.RemoteSources{})
	_, err = e.runAs(testUser, remoteBackupDef(repo.GetId(), photos, "Pictures", apiMirror))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "worker_id") != `worker "worker" must be upgraded to back up a folder of a remote` {
		t.Errorf("an older worker = %v (%v)", err, violations(t, err))
	}
	// A backup of paths is not concerned.
	if _, err := e.runAs(testUser, apiBackupDef(repo.GetId())); err != nil {
		t.Errorf("a backup of paths on that worker: %v", err)
	}
}

// TestScheduleRemoteSource: a schedule of a backup of a remote targets a
// worker, is checked against its repository like RunJob, and is saved
// even for a worker that cannot use the method yet (its runs fail at
// dispatch with tool_unavailable).
func TestScheduleRemoteSource(t *testing.T) {
	t.Parallel()
	late := &lateSchedules{}
	re := newResticEnv(t, func(d *Deps) { d.Schedules = late })
	e := lateScheduleEnv(t, re.testEnv, late)
	ctx := t.Context()
	repo := re.createRepo("vault")
	photos := re.addRemote(testUser, "Photos", "s3")
	create := func(in *syncrv1.ScheduleInput) error {
		_, err := e.schedulesFor(testUser).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
		return err
	}

	re.setWorkerSources(link.Features(), workers.RemoteSources{Mirror: true, MountReason: "/dev/fuse is missing"})
	in := e.scheduleInput("Photos nightly")
	in.Definition = remoteBackupDef(repo.GetId(), photos, "Pictures", apiMount)
	sc := e.createSchedule(testUser, in)
	if !proto.Equal(sc.GetDefinition(), in.GetDefinition()) || sc.GetRepository().GetName() != "vault" {
		t.Errorf("schedule = %v", sc)
	}

	onLabel := e.scheduleInput("On a label")
	onLabel.Definition = remoteBackupDef(repo.GetId(), photos, "Pictures", apiMirror)
	onLabel.Target = &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: "nas"}}
	err := create(onLabel)
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "target.worker_label") != jobkind.RemoteSourceSingleWorkerReason {
		t.Errorf("a label target = %v (%v)", err, violations(t, err))
	}
	// A backup of paths may still target a label.
	onLabel.Definition = apiBackupDef(repo.GetId())
	if err := create(onLabel); err != nil {
		t.Errorf("a backup of paths on a label: %v", err)
	}

	overlap := e.scheduleInput("Overlap")
	overlap.Definition = remoteBackupDef(repo.GetId(), re.remote, "restic", apiMirror)
	err = create(overlap)
	if codeOf(t, err) != connect.CodeInvalidArgument ||
		violation(err, "definition.restic_backup.remote_source.path") != `holds repository "vault" (at "restic/vault"): a backup cannot read its own repository; choose a folder beside it` {
		t.Errorf("an overlapping schedule = %v (%v)", err, violations(t, err))
	}
	theirs := re.addRemote(testOther, "Theirs", "s3")
	foreign := e.scheduleInput("Foreign")
	foreign.Definition = remoteBackupDef(repo.GetId(), theirs, "", apiMirror)
	if err := create(foreign); codeOf(t, err) != connect.CodeNotFound {
		t.Errorf("another user's remote = %v", err)
	}
}

func TestWorkerInfoRemoteSources(t *testing.T) {
	t.Parallel()
	base := workers.Info{Version: "v2.2.0", ReportedAt: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	for _, tt := range []struct {
		name string
		info func(i *workers.Info)
		want *syncrv1.RemoteSourceSupport
	}{
		{"an older worker", func(*workers.Info) {}, nil},
		{"with the feature but nothing available", func(i *workers.Info) { i.Features = []string{link.FeatureResticRemoteSource} },
			&syncrv1.RemoteSourceSupport{}},
		{"mirror only", func(i *workers.Info) {
			i.Features = []string{link.FeatureResticRemoteSource}
			i.RemoteSources = workers.RemoteSources{Mirror: true, MountReason: "/dev/fuse is missing"}
		}, &syncrv1.RemoteSourceSupport{Mirror: true, MountReason: "/dev/fuse is missing"}},
	} {
		info := base
		tt.info(&info)
		if got := workerInfoToProto(info).GetRemoteSources(); !proto.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
			t.Errorf("%s: remote_sources = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestProgressPhase(t *testing.T) {
	t.Parallel()
	got := progressToProto(jobs.Sample{Time: time.Unix(1, 0), Phase: link.ProgressPhaseMirror, Stats: jobs.Stats{Bytes: 3}})
	if got.GetPhase() != "mirror" || got.GetStats().GetBytes() != 3 {
		t.Errorf("progress = %v", got)
	}
	if got := progressToProto(jobs.Sample{}); got.GetPhase() != "" {
		t.Errorf("a sample without a phase = %v", got)
	}
}
