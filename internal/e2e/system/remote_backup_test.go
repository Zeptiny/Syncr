//go:build integration

package system

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
)

// remoteBackupJob backs up the folder path of a remote into a repository
// with method, leaving out *.tmp files.
func remoteBackupJob(repo, remote, path string, method syncrv1.ResticRemoteSourceMethod) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
		RepositoryId: repo, Tags: []string{"e2e"}, Host: "e2e-host",
		RemoteSource: &syncrv1.ResticRemoteSourceDefinition{
			RemoteId: remote, Path: path, Method: method,
			Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}},
		},
	}}}
}

// TestSystemRemoteBackupToMinIO backs up a folder of a remote (MinIO)
// into a repository on the same remote through the API with the real
// binaries, with the mirror method: the snapshot holds the folder's
// contents at its root and carries the remote's tag, the job's summary
// shows the mirror's counters, and a second run reads nothing again. The
// API refuses a source that holds the repository, a Local directory
// remote, a schedule of it on a label, and a worker that cannot use the
// method (with the worker's reason); a schedule on the worker is saved.
func TestSystemRemoteBackupToMinIO(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t, false)
	c, w, ctx := e.c, e.w, t.Context()
	const mirror = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR
	const mount = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT

	info := c.waitOnline(w.id).GetInfo()
	if !slices.Contains(info.GetFeatures(), link.FeatureResticRemoteSource) || !info.GetRemoteSources().GetMirror() {
		t.Fatalf("the worker reports %v and remote sources %v", info.GetFeatures(), info.GetRemoteSources())
	}
	canMount := info.GetRemoteSources().GetMount()
	if !canMount && info.GetRemoteSources().GetMountReason() == "" {
		t.Errorf("the worker cannot mount and gives no reason: %v", info.GetRemoteSources())
	}

	endpoint := startMinIO(t)
	minio := c.createRemote(&syncrv1.RemoteInput{
		Name: "MinIO", BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "bucket": "syncr-remote-backup", "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	})
	repo, err := e.create("Vault", minio, "restic/vault", resticPassword, syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	// The folder to back up, put on the remote by a copy job.
	src := filepath.Join(w.root, "photos")
	files := map[string]string{"a.jpg": "first photo", "2026/b.jpg": "second photo", "2026/03/c b.jpg": "third"}
	for name, content := range files {
		write(t, filepath.Join(src, name), content)
	}
	write(t, filepath.Join(src, "upload.tmp"), "left out by the filter")
	e.run(copyDef(local(src), onRemote(minio, "Photos"), 0))

	// 1. The first backup mirrors the folder and backs it up.
	first := e.run(remoteBackupJob(repo.GetId(), minio, "Photos", mirror))
	out := first.GetOutput().GetResticBackup()
	if len(out.GetSnapshotId()) != 64 || out.GetFilesNew() != int64(len(files)) {
		t.Errorf("backup output = %v, want a snapshot of %d new files", out, len(files))
	}
	if st := out.GetRemoteSource(); st.GetMethod() != mirror || st.GetMirroredFiles() != int64(len(files)) || st.GetMirrorDiscarded() {
		t.Errorf("the mirror's counters = %v, want %d files mirrored", st, len(files))
	}
	if first.GetTitle() != "Back up MinIO/Photos (mirror) → Vault" || detail(first, "Read through") != "Mirror" ||
		!strings.HasPrefix(detail(first, "Mirrored"), "28 bytes, 3 files, 0 deleted") || detail(first, "Snapshot") == "" {
		t.Errorf("job %q details %v", first.GetTitle(), first.GetDetails())
	}

	// 2. The snapshot: the folder's contents at its root, tagged with the
	// remote.
	list := e.snapshots(repo.GetId())
	if len(list.GetSnapshots()) != 1 {
		t.Fatalf("snapshots = %v", list)
	}
	snap := list.GetSnapshots()[0]
	if want := []string{"e2e", link.RemoteSourceTagPrefix + minio}; !slices.Equal(snap.GetTags(), want) || snap.GetHost() != "e2e-host" {
		t.Errorf("snapshot tags %v host %q, want %v and e2e-host", snap.GetTags(), snap.GetHost(), want)
	}
	if got := e.browse(repo.GetId(), snap.GetId(), ""); !slices.Equal(got, []string{"2026", "a.jpg"}) {
		t.Errorf("the snapshot's root = %q, want the folder's contents", got)
	}
	// The mirror lives in the worker's data directory, under one key.
	mirrors, err := os.ReadDir(filepath.Join(w.dir, "state", "sources", "mirror"))
	if err != nil || len(mirrors) != 1 || len(mirrors[0].Name()) != link.SourceKeyLen {
		t.Errorf("mirrors on the worker = %v (%v)", mirrors, err)
	}

	// 3. A second run reads nothing again.
	second := e.run(remoteBackupJob(repo.GetId(), minio, "Photos", mirror))
	if o := second.GetOutput().GetResticBackup(); o.GetFilesUnmodified() != int64(len(files)) || o.GetFilesNew() != 0 ||
		o.GetRemoteSource().GetMirroredFiles() != 0 {
		t.Errorf("the second backup = %v, want every file unmodified and nothing mirrored", o)
	}

	// 4. Refusals through the API.
	refused := func(def *syncrv1.JobDefinition) error {
		_, err := c.jobs.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: w.id, Definition: def}))
		return err
	}
	err = refused(remoteBackupJob(repo.GetId(), minio, "restic", mirror))
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!slices.Contains(violations(err), `definition.restic_backup.remote_source.path: holds repository "Vault" (at "restic/vault"): a backup cannot read its own repository; choose a folder beside it`) {
		t.Errorf("a source holding the repository = %v (%q)", err, violations(err))
	}
	disk := c.createRemote(&syncrv1.RemoteInput{Name: "Disk", BackendType: "local", Options: map[string]string{"path": src}})
	err = refused(remoteBackupJob(repo.GetId(), disk, "", mirror))
	if !slices.Contains(violations(err), "definition.restic_backup.remote_source.remote_id: this remote is a folder on a worker; back up that folder instead") {
		t.Errorf("a Local directory remote = %v (%q)", err, violations(err))
	}
	if !canMount {
		err = refused(remoteBackupJob(repo.GetId(), minio, "Photos", mount))
		if want := "worker_id: worker " + `"` + t.Name() + `" cannot mount a remote: ` + info.GetRemoteSources().GetMountReason(); !slices.Contains(violations(err), want) {
			t.Errorf("a mount on a worker that cannot = %v (%q), want %q", err, violations(err), want)
		}
	}

	// 5. Schedules target the worker, never a label.
	schedules := syncrv1connect.NewScheduleServiceClient(c.client(), c.base)
	schedule := func(name string, target *syncrv1.ScheduleTarget) error {
		_, err := schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: &syncrv1.ScheduleInput{
			Name: name, Cron: "0 3 1 1 *", Timezone: "UTC", Enabled: true, Target: target,
			MissedRun:  &syncrv1.MissedRunPolicy{Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE, MaxDelay: durationpb.New(time.Hour)},
			Overlap:    syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP,
			Definition: remoteBackupJob(repo.GetId(), minio, "Photos", mount),
		}}))
		return err
	}
	if err := schedule("Photos on a label", &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: "nas"}}); !slices.Contains(violations(err),
		"target.worker_label: a backup of a remote keeps its mirror or mount folder on one worker; choose a worker") {
		t.Errorf("a schedule on a label = %v (%q)", err, violations(err))
	}
	// On the worker it is saved even when the worker cannot mount: its
	// runs would fail with tool_unavailable.
	if err := schedule("Photos nightly", &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: w.id}}); err != nil {
		t.Errorf("a schedule on the worker: %v", err)
	}

	// 6. With the mirror turned off in its configuration, the worker says
	// so, and RunJob refuses it with that reason.
	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, w.config, "remote_sources:\n  mirror: false\n")
	w.start()
	deadline := time.Now().Add(waitLong)
	for c.waitOnline(w.id).GetInfo().GetRemoteSources().GetMirror() {
		if time.Now().After(deadline) {
			t.Fatal("the worker still reports the mirror after its restart")
		}
		time.Sleep(100 * time.Millisecond)
	}
	err = refused(remoteBackupJob(repo.GetId(), minio, "Photos", mirror))
	if want := `worker_id: worker "` + t.Name() + `" cannot keep a mirror of a remote: remote_sources.mirror is off in worker.yaml`; !slices.Contains(violations(err), want) {
		t.Errorf("a mirror on a worker without = %v (%q), want %q", err, violations(err), want)
	}

	e.noPasswordAnywhere()
	if strings.Contains(w.proc.out.String(), minioPassword) || strings.Contains(c.proc.out.String(), minioPassword) {
		t.Error("the S3 secret appears in a log")
	}
}
