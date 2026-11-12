//go:build integration

package system

import (
	"fmt"
	"net/http"
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

// resticPassword is the repositories' password in these tests: a sentinel
// that must appear in no API response and in neither process's log.
const resticPassword = "e2e-restic-password-sentinel"

// resticEnv is a controller with one worker and the clients of the restic
// and probe services, as the admin.
type resticEnv struct {
	t      *testing.T
	c      *controller
	w      *worker
	restic syncrv1connect.ResticServiceClient
	probes syncrv1connect.ProbeServiceClient
	// seen collects the text of responses, to search for the password.
	seen []string
}

func newResticEnv(t *testing.T, passive bool) *resticEnv {
	t.Helper()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, passive)
	w.start()
	info := c.waitOnline(w.id)
	for _, f := range []string{"probe", "restic.v2", "restic.delete"} {
		if !slices.Contains(info.GetInfo().GetFeatures(), f) {
			t.Fatalf("the worker does not report the feature %q: %v", f, info.GetInfo().GetFeatures())
		}
	}
	if got, want := info.GetInfo().GetProbeOps(), link.ProbeOps(); !slices.Equal(got, want) {
		t.Fatalf("the worker reports the probe operations %v, want all of %v", got, want)
	}
	return &resticEnv{
		t: t, c: c, w: w,
		restic: syncrv1connect.NewResticServiceClient(c.client(), c.base),
		probes: syncrv1connect.NewProbeServiceClient(c.client(), c.base),
	}
}

// note remembers a response's text for the final password search.
func (e *resticEnv) note(msg fmt.Stringer) { e.seen = append(e.seen, msg.String()) }

// create creates or connects a repository.
func (e *resticEnv) create(name, remote, path, password string, mode syncrv1.RepositoryCreateMode) (*syncrv1.Repository, error) {
	resp, err := e.restic.CreateRepository(e.t.Context(), connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: &syncrv1.RepositoryInput{
		Name: name, RemoteId: remote, Path: path, Password: password, Mode: mode, WorkerId: e.w.id,
	}}))
	if err != nil {
		return nil, err
	}
	e.note(resp.Msg)
	return resp.Msg.GetRepository(), nil
}

// run runs a job to its end and fails unless it succeeded.
func (e *resticEnv) run(def *syncrv1.JobDefinition) *syncrv1.Job {
	e.t.Helper()
	j, _ := e.c.watch(e.c.runJob(e.w.id, def))
	e.note(j)
	wantStatus(e.t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	return j
}

// snapshots lists a repository's snapshots through the worker.
func (e *resticEnv) snapshots(repo string) *syncrv1.ListSnapshotsResponse {
	e.t.Helper()
	resp, err := e.restic.ListSnapshots(e.t.Context(), connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: repo, Refresh: true}))
	if err != nil {
		e.t.Fatalf("ListSnapshots: %v", err)
	}
	if resp.Msg.GetRefreshError() != "" {
		e.t.Fatalf("ListSnapshots: the refresh failed: %s", resp.Msg.GetRefreshError())
	}
	e.note(resp.Msg)
	return resp.Msg
}

// browse lists a folder of a snapshot and returns the names.
func (e *resticEnv) browse(repo, snapshot, path string) []string {
	e.t.Helper()
	resp, err := e.restic.BrowseSnapshot(e.t.Context(), connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: repo, SnapshotId: snapshot, Path: path}))
	if err != nil {
		e.t.Fatalf("BrowseSnapshot(%s): %v", path, err)
	}
	e.note(resp.Msg)
	var names []string
	for _, en := range resp.Msg.GetEntries() {
		names = append(names, en.GetName())
	}
	return names
}

// noPasswordAnywhere fails if the repositories' password is in a response
// seen so far or in a process's log.
func (e *resticEnv) noPasswordAnywhere() {
	e.t.Helper()
	for _, text := range e.seen {
		if strings.Contains(text, resticPassword) {
			e.t.Errorf("a response contains the repository password: %s", text)
		}
	}
	if strings.Contains(e.w.proc.out.String(), resticPassword) {
		e.t.Error("the repository password appears in the worker's log")
	}
	if strings.Contains(e.c.proc.out.String(), resticPassword) {
		e.t.Error("the repository password appears in the controller's log")
	}
}

func backupJob(repo string, paths []string, excludes, tags []string) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
		RepositoryId: repo, Paths: paths, Excludes: excludes, Tags: tags, Host: "e2e-host",
	}}}
}

func restoreJob(d *syncrv1.ResticRestoreDefinition) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: d}}
}

// detail returns the value of a job's display detail, "" if it has none.
func detail(j *syncrv1.Job, label string) string {
	for _, d := range j.GetDetails() {
		if d.GetLabel() == label {
			return d.GetValue()
		}
	}
	return ""
}

// write writes content to path, creating its folder.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// read returns a file's content.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// secondUser creates another user and returns an HTTP client signed in as
// them (admin makes them an administrator).
func (e *resticEnv) secondUser(name string, admin bool) *http.Client {
	e.t.Helper()
	ctx := e.t.Context()
	role := syncrv1.UserRole_USER_ROLE_USER
	if admin {
		role = syncrv1.UserRole_USER_ROLE_ADMIN
	}
	const password = "e2e-second-user-password"
	if _, err := syncrv1connect.NewUserServiceClient(e.c.session, e.c.base).CreateUser(ctx, connect.NewRequest(&syncrv1.CreateUserRequest{
		Username: name, DisplayName: name, Password: password, Role: role,
	})); err != nil {
		e.t.Fatalf("CreateUser(%s): %v", name, err)
	}
	req := connect.NewRequest(&syncrv1.LoginRequest{Username: name, Password: password})
	req.Header().Set(csrfHeader, "1")
	resp, err := syncrv1connect.NewAuthServiceClient(http.DefaultClient, e.c.base).Login(ctx, req)
	if err != nil {
		e.t.Fatalf("Login(%s): %v", name, err)
	}
	var cookie *http.Cookie
	for _, line := range resp.Header().Values("Set-Cookie") {
		if ck, err := http.ParseSetCookie(line); err == nil && ck.Value != "" {
			cookie = ck
		}
	}
	if cookie == nil {
		e.t.Fatalf("Login(%s) set no cookie", name)
	}
	return &http.Client{Transport: headers{next: http.DefaultTransport, set: func(h http.Header) {
		h.Set("Cookie", cookie.Name+"="+cookie.Value)
		h.Set(csrfHeader, "1")
	}}}
}

// TestSystemResticToMinIO runs restic end to end through the API with the
// real binaries: a repository on an S3 remote (MinIO) is created and
// connected, backed up, listed, browsed, restored in every form, pruned by
// a retention policy and checked; two backups queued at once run one after
// the other; the remote is tested; other users reach nothing; a second
// repository is deleted with its data while unrelated files next to it
// survive; and the repository password appears in no response and no log.
func TestSystemResticToMinIO(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t, false)
	c, w, ctx := e.c, e.w, t.Context()
	endpoint := startMinIO(t)
	minio := c.createRemote(&syncrv1.RemoteInput{
		Name: "MinIO", BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "bucket": "syncr-e2e", "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	})
	const (
		initMode    = syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT
		connectMode = syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_CONNECT
	)

	// 1. A new repository.
	repo, err := e.create("Vault", minio, "restic/vault", resticPassword, initMode)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if len(repo.GetResticId()) != 64 || repo.GetStats().GetSnapshotCount() != 0 || repo.GetRemote().GetName() != "MinIO" {
		t.Fatalf("the new repository = %v", repo)
	}

	// 2. The location is taken; forgotten and connected again, it is the
	// same repository, and only with its password.
	if _, err := e.create("Twin", minio, "restic/vault", resticPassword, initMode); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("a second repository at the same location = %v, want AlreadyExists", err)
	}
	if _, err := e.restic.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: repo.GetId()})); err != nil {
		t.Fatalf("DeleteRepository(forget only): %v", err)
	}
	if _, err := e.create("Vault", minio, "restic/vault", "not the password", connectMode); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "password") {
		t.Errorf("connect with a wrong password = %v, want a field error on the password", err)
	}
	if _, err := e.create("Vault", minio, "restic/vault", resticPassword, initMode); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("init over an existing repository = %v, want a hint to connect", err)
	}
	again, err := e.create("Vault", minio, "restic/vault", resticPassword, connectMode)
	if err != nil || again.GetResticId() != repo.GetResticId() || again.GetId() == repo.GetId() {
		t.Fatalf("connect = %v (%v), want the same restic ID %s under a new ID", again, err, repo.GetResticId())
	}
	repo = again

	// 3. and 4. A backup with an exclude and a tag.
	src := filepath.Join(w.root, "src")
	files := map[string]string{
		"a b.txt": "a b", "[x]*?.txt": "brackets", "ü.txt": "umlaut", "docs/one.txt": "one", "docs/deep/two.txt": "two",
	}
	for name, content := range files {
		write(t, filepath.Join(src, name), content)
	}
	write(t, filepath.Join(src, "skip.tmp"), "excluded")
	first := e.run(backupJob(repo.GetId(), []string{src}, []string{"*.tmp"}, []string{"e2e"}))
	out := first.GetOutput().GetResticBackup()
	if len(out.GetSnapshotId()) != 64 || out.GetFilesNew() != int64(len(files)) {
		t.Errorf("backup output = %v, want a snapshot and %d new files", out, len(files))
	}
	if !strings.HasPrefix(first.GetTitle(), "Back up") || first.GetRepository().GetName() != "Vault" || detail(first, "Snapshot") == "" {
		t.Errorf("backup job: title %q, repository %v, details %v", first.GetTitle(), first.GetRepository(), first.GetDetails())
	}

	// 5. The snapshot, and its tree without the excluded file.
	list := e.snapshots(repo.GetId())
	if len(list.GetSnapshots()) != 1 || list.GetStale() || !slices.Equal(list.GetSnapshots()[0].GetTags(), []string{"e2e"}) ||
		list.GetSnapshots()[0].GetSummary().GetTotalFilesProcessed() != int64(len(files)) || list.GetSnapshots()[0].GetHost() != "e2e-host" {
		t.Fatalf("snapshots = %v", list)
	}
	snap := list.GetSnapshots()[0].GetId()
	if snap != out.GetSnapshotId() {
		t.Errorf("the listed snapshot %s is not the backup's %s", snap, out.GetSnapshotId())
	}
	tree := filepath.ToSlash(src) // on Linux the tree path is the path
	if got := e.browse(repo.GetId(), snap, tree); !slices.Equal(got, []string{"docs", "[x]*?.txt", "a b.txt", "ü.txt"}) {
		t.Errorf("the snapshot's %s = %q", tree, got)
	}
	if top := e.browse(repo.GetId(), snap, ""); len(top) != 1 {
		t.Errorf("the snapshot's top = %q", top)
	}

	// 6. Two backups queued at once run one after the other.
	write(t, filepath.Join(src, "a b.txt"), "a b, changed")
	idA := c.runJob(w.id, backupJob(repo.GetId(), []string{src}, []string{"*.tmp"}, nil))
	idB := c.runJob(w.id, backupJob(repo.GetId(), []string{src}, []string{"*.tmp"}, nil))
	queued, err := c.jobs.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: idB}))
	if err != nil {
		t.Fatal(err)
	}
	if st := queued.Msg.GetJob().GetStatus(); st == syncrv1.JobStatus_JOB_STATUS_QUEUED && queued.Msg.GetJob().GetWaitingForJobId() != idA {
		t.Errorf("the second backup waits for %q, want the first %s", queued.Msg.GetJob().GetWaitingForJobId(), idA)
	}
	a, _ := c.watch(idA)
	b, _ := c.watch(idB)
	wantStatus(t, a, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	wantStatus(t, b, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	e.note(a)
	e.note(b)
	if b.GetStartedAt().AsTime().Before(a.GetFinishedAt().AsTime()) {
		t.Errorf("the second backup started at %v, before the first finished at %v", b.GetStartedAt().AsTime(), a.GetFinishedAt().AsTime())
	}
	if a.GetOutput().GetResticBackup().GetFilesChanged() != 1 || b.GetOutput().GetResticBackup().GetFilesChanged() != 0 || b.GetWaitingForJobId() != "" {
		t.Errorf("changed files: first %v, second %v", a.GetOutput().GetResticBackup(), b.GetOutput().GetResticBackup())
	}

	// 7. The whole first snapshot.
	r1 := filepath.Join(w.root, "r1")
	whole := e.run(restoreJob(&syncrv1.ResticRestoreDefinition{RepositoryId: repo.GetId(), SnapshotId: snap, Target: r1}))
	if whole.GetOutput().GetResticRestore().GetBytesRestored() == 0 {
		t.Errorf("restore output = %v", whole.GetOutput())
	}
	for name, content := range files {
		if got := read(t, filepath.Join(r1, src, name)); got != content {
			t.Errorf("restored %s = %q, want %q (the first snapshot's content)", name, got, content)
		}
	}
	if _, err := os.Stat(filepath.Join(r1, src, "skip.tmp")); !os.IsNotExist(err) {
		t.Errorf("the excluded file was restored (%v)", err)
	}

	// 8. Selected items only, with a name full of pattern characters.
	r2 := filepath.Join(w.root, "r2")
	e.run(restoreJob(&syncrv1.ResticRestoreDefinition{
		RepositoryId: repo.GetId(), SnapshotId: snap, Target: r2, Paths: []string{tree + "/docs", tree + "/[x]*?.txt"},
	}))
	var restored []string
	if err := filepath.WalkDir(filepath.Join(r2, src), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(r2, src), p)
			restored = append(restored, filepath.ToSlash(rel))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(restored)
	if !slices.Equal(restored, []string{"[x]*?.txt", "docs/deep/two.txt", "docs/one.txt"}) {
		t.Errorf("the selected restore wrote %q", restored)
	}

	// 9. One folder into a target that already has a file: kept by default,
	// replaced when asked.
	r3 := filepath.Join(w.root, "r3")
	write(t, filepath.Join(r3, "a b.txt"), "mine")
	e.run(restoreJob(&syncrv1.ResticRestoreDefinition{RepositoryId: repo.GetId(), SnapshotId: snap, Target: r3, Subfolder: tree}))
	if got := read(t, filepath.Join(r3, "a b.txt")); got != "mine" {
		t.Errorf("the default restore replaced an existing file: %q", got)
	}
	if got := read(t, filepath.Join(r3, "docs", "one.txt")); got != "one" {
		t.Errorf("the folder restore wrote docs/one.txt = %q", got)
	}
	e.run(restoreJob(&syncrv1.ResticRestoreDefinition{
		RepositoryId: repo.GetId(), SnapshotId: snap, Target: r3, Subfolder: tree,
		Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_IF_CHANGED,
	}))
	if got := read(t, filepath.Join(r3, "a b.txt")); got != "a b" {
		t.Errorf("the if-changed restore left %q", got)
	}

	// 10. Retention with prune: one snapshot stays, and the list knows it
	// is out of date until it is refreshed.
	forget := e.run(&syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticForget{ResticForget: &syncrv1.ResticForgetDefinition{
		RepositoryId: repo.GetId(), Policy: &syncrv1.ResticRetentionPolicy{KeepLast: 1}, Prune: true,
	}}})
	if fo := forget.GetOutput().GetResticForget(); len(fo.GetRemovedSnapshotIds()) != 2 || fo.GetKeptCount() != 1 {
		t.Errorf("forget output = %v, want 2 removed and 1 kept", fo)
	}
	stale, err := e.restic.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: repo.GetId()}))
	if err != nil || !stale.Msg.GetStale() || len(stale.Msg.GetSnapshots()) != 1 {
		// The cache was last filled after the first backup: one snapshot,
		// and three jobs changed the repository since.
		t.Errorf("the cached list = %v (%v), want stale", stale, err)
	}
	if after := e.snapshots(repo.GetId()); len(after.GetSnapshots()) != 1 || after.GetSnapshots()[0].GetId() == snap {
		t.Errorf("after retention: %v", after)
	}

	// 11. A full check.
	check := e.run(&syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{
		RepositoryId: repo.GetId(), ReadDataPercent: 100,
	}}})
	if co := check.GetOutput().GetResticCheck(); co.GetNumErrors() != 0 || co.GetReadDataPercent() != 100 {
		t.Errorf("check output = %v", co)
	}

	// 12. The remote answers; S3 cannot tell its usage.
	test, err := e.probes.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{RemoteId: minio}))
	if err != nil || !test.Msg.GetOk() || !test.Msg.GetUsage().GetUnsupported() || test.Msg.GetWorker().GetId() != w.id {
		t.Errorf("TestRemote = %v (%v)", test, err)
	}
	listed, err := e.probes.ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: minio, Path: "restic/vault"}))
	if err != nil || len(listed.Msg.GetEntries()) < 5 {
		t.Errorf("ListRemoteFiles(the repository) = %v (%v)", listed, err)
	}

	// 13. Another user, and another administrator, reach none of it.
	for name, admin := range map[string]bool{"bob": false, "carol": true} {
		client := e.secondUser(name, admin)
		rc := syncrv1connect.NewResticServiceClient(client, c.base)
		for what, call := range map[string]func() error{
			"GetRepository": func() error {
				_, err := rc.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: repo.GetId()}))
				return err
			},
			"ListSnapshots": func() error {
				_, err := rc.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: repo.GetId()}))
				return err
			},
			"BrowseSnapshot": func() error {
				_, err := rc.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: repo.GetId(), SnapshotId: snap}))
				return err
			},
			"UpdateRepository": func() error {
				_, err := rc.UpdateRepository(ctx, connect.NewRequest(&syncrv1.UpdateRepositoryRequest{RepositoryId: repo.GetId(), Version: repo.GetVersion(), Name: "Theirs"}))
				return err
			},
			"DeleteRepository with its data": func() error {
				_, err := rc.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: repo.GetId(), DeleteData: true, ConfirmName: "Vault"}))
				return err
			},
			"RunJob on it": func() error {
				_, err := syncrv1connect.NewJobServiceClient(client, c.base).RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{
					WorkerId: w.id, Definition: backupJob(repo.GetId(), []string{src}, nil, nil),
				}))
				return err
			},
			"ListRemoteFiles of its remote": func() error {
				_, err := syncrv1connect.NewProbeServiceClient(client, c.base).ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: minio}))
				return err
			},
		} {
			if code := connect.CodeOf(call()); code != connect.CodeNotFound && code != connect.CodeInvalidArgument {
				// A job on an unknown repository is a field error on its
				// definition; everything else is NotFound.
				t.Errorf("%s: %s = %v, want NotFound", name, what, code)
			}
		}
	}
	if still, err := e.restic.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: repo.GetId()})); err != nil ||
		still.Msg.GetRepository().GetDeleting() || still.Msg.GetRepository().GetName() != "Vault" {
		t.Errorf("the repository after the other users' attempts = %v (%v)", still, err)
	}

	// 14. A second repository is deleted with its data; a file next to it
	// that restic did not write survives.
	doomed, err := e.create("Doomed", minio, "shared/doomed", resticPassword, initMode)
	if err != nil {
		t.Fatalf("CreateRepository(Doomed): %v", err)
	}
	write(t, filepath.Join(w.root, "keep", "keep-me.txt"), "not restic's")
	j, _ := c.watch(c.runJob(w.id, copyDef(local(filepath.Join(w.root, "keep")), onRemote(minio, "shared/doomed"), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	schedules := syncrv1connect.NewScheduleServiceClient(c.client(), c.base)
	scheduleInput := func(name string) *syncrv1.ScheduleInput {
		return &syncrv1.ScheduleInput{
			Name: name, Cron: "0 3 1 1 *", Timezone: "UTC", Enabled: true,
			Target:     &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: w.id}},
			MissedRun:  &syncrv1.MissedRunPolicy{Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE, MaxDelay: durationpb.New(time.Hour)},
			Overlap:    syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP,
			Definition: backupJob(doomed.GetId(), []string{src}, nil, nil),
		}
	}
	sched, err := schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: scheduleInput("Yearly doomed backup")}))
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if got := sched.Msg.GetSchedule().GetRepository().GetName(); got != "Doomed" {
		t.Errorf("the schedule's repository = %q", got)
	}
	e.run(backupJob(doomed.GetId(), []string{src}, nil, nil))
	last := c.runJob(w.id, backupJob(doomed.GetId(), []string{src}, nil, nil))

	deleteDoomed := func(confirm string) (*connect.Response[syncrv1.DeleteRepositoryResponse], error) {
		return e.restic.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{
			RepositoryId: doomed.GetId(), DeleteData: true, ConfirmName: confirm, WorkerId: w.id,
		}))
	}
	if _, err := deleteDoomed("wrong"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("DeleteRepository with the wrong name = %v, want InvalidArgument", err)
	}
	deleted, err := deleteDoomed("Doomed")
	if err != nil || deleted.Msg.GetDeletionJobId() == "" || deleted.Msg.GetWorker().GetId() != w.id {
		t.Fatalf("DeleteRepository with its data = %v (%v)", deleted, err)
	}
	// While it is deleting: the schedule is paused and nothing new starts.
	// (The deletion may already have finished on a fast machine: then the
	// repository is gone, which the calls below report as NotFound.)
	if during, err := e.restic.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: doomed.GetId()})); err == nil {
		if !during.Msg.GetRepository().GetDeleting() || during.Msg.GetRepository().GetDeletionJobId() != deleted.Msg.GetDeletionJobId() {
			t.Errorf("the repository while deleting = %v", during.Msg.GetRepository())
		}
	}
	paused, err := schedules.GetSchedule(ctx, connect.NewRequest(&syncrv1.GetScheduleRequest{ScheduleId: sched.Msg.GetSchedule().GetId()}))
	if err != nil || paused.Msg.GetSchedule().GetEnabled() || paused.Msg.GetSchedule().GetPausedReason() != "repository_deleting" {
		t.Errorf("the schedule after the deletion started = %v (%v)", paused, err)
	}
	if _, err := c.jobs.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: w.id, Definition: backupJob(doomed.GetId(), []string{src}, nil, nil)})); err == nil {
		t.Error("a backup started on a repository whose data is being deleted")
	}
	if _, err := schedules.CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: scheduleInput("Too late")})); err == nil {
		t.Error("a schedule was created on a repository whose data is being deleted")
	}
	if _, err := c.jobs.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: w.id, Definition: &syncrv1.JobDefinition{
		Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: repo.GetId()}},
	}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("RunJob(restic_delete_data) = %v, want InvalidArgument", err)
	}
	// The backup queued before the deletion runs first.
	before, _ := c.watch(last)
	wantStatus(t, before, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	gone, _ := c.watch(deleted.Msg.GetDeletionJobId())
	e.note(gone)
	wantStatus(t, gone, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	if gone.GetStartedAt().AsTime().Before(before.GetFinishedAt().AsTime()) {
		t.Errorf("the deletion started at %v, before the queued backup finished at %v", gone.GetStartedAt().AsTime(), before.GetFinishedAt().AsTime())
	}
	if do := gone.GetOutput().GetResticDeleteData(); do.GetDirectoryRemoved() || do.GetAlreadyGone() || do.GetDeletedObjects() == 0 {
		t.Errorf("deletion output = %v, want objects deleted and the folder kept (it holds another file)", do)
	}
	// The repository disappears once the controller has seen the job end.
	deadline := time.Now().Add(waitLong)
	for {
		_, err := e.restic.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: doomed.GetId()}))
		if connect.CodeOf(err) == connect.CodeNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the repository is still there after its data was deleted (%v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	left, err := e.probes.ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: minio, Path: "shared/doomed"}))
	if err != nil || len(left.Msg.GetEntries()) != 1 || left.Msg.GetEntries()[0].GetName() != "keep-me.txt" {
		t.Errorf("what is left at the deleted repository's location = %v (%v), want only keep-me.txt", left, err)
	}
	// The other repository is untouched, and still holds the remote.
	if after := e.snapshots(repo.GetId()); len(after.GetSnapshots()) != 1 {
		t.Errorf("the first repository after the deletion: %v", after)
	}
	if _, err := c.remotes.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: minio})); err == nil {
		t.Error("a remote that holds a repository was deleted")
	}

	e.noPasswordAnywhere()
	if strings.Contains(w.proc.out.String(), minioPassword) || strings.Contains(c.proc.out.String(), minioPassword) {
		t.Error("the S3 secret appears in a log")
	}
}

// TestSystemPassiveProbe: probes reach a passive worker through the
// controller's calls to it: its folders, a repository on its own disk,
// that repository's snapshots and a snapshot's tree.
func TestSystemPassiveProbe(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t, true)
	c, w, ctx := e.c, e.w, t.Context()

	write(t, filepath.Join(w.root, "src", "notes", "a.txt"), "a")
	write(t, filepath.Join(w.root, "src", "b.txt"), "b")
	roots, err := e.probes.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: w.id}))
	if err != nil || len(roots.Msg.GetEntries()) != 1 || roots.Msg.GetEntries()[0].GetPath() != w.root {
		t.Fatalf("ListWorkerFiles(roots) = %v (%v)", roots, err)
	}
	inside, err := e.probes.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: w.id, Path: filepath.Join(w.root, "src")}))
	if err != nil || len(inside.Msg.GetEntries()) != 2 || inside.Msg.GetEntries()[0].GetName() != "notes" ||
		inside.Msg.GetEntries()[0].GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
		t.Fatalf("ListWorkerFiles(src) = %v (%v), want the folder first", inside, err)
	}
	if _, err := e.probes.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: w.id, Path: filepath.Dir(w.root)})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("ListWorkerFiles(outside the roots) = %v, want InvalidArgument", err)
	}

	// A repository on the worker's own disk needs that worker named.
	disk := c.createRemote(&syncrv1.RemoteInput{Name: "Worker disk", BackendType: "local", Options: map[string]string{"path": w.root}})
	repo, err := e.create("On disk", disk, "repo", resticPassword, syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.root, "repo", "config")); err != nil {
		t.Errorf("the repository was not created under the worker's root: %v", err)
	}
	src := filepath.Join(w.root, "src")
	e.run(backupJob(repo.GetId(), []string{src}, nil, nil))
	list, err := e.restic.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: repo.GetId(), Refresh: true, WorkerId: w.id}))
	if err != nil || len(list.Msg.GetSnapshots()) != 1 || list.Msg.GetRefreshError() != "" || list.Msg.GetWorker().GetId() != w.id {
		t.Fatalf("ListSnapshots = %v (%v)", list, err)
	}
	e.note(list.Msg)
	tree, err := e.restic.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{
		RepositoryId: repo.GetId(), SnapshotId: list.Msg.GetSnapshots()[0].GetId(), Path: filepath.ToSlash(src), WorkerId: w.id,
	}))
	if err != nil || len(tree.Msg.GetEntries()) != 2 {
		t.Fatalf("BrowseSnapshot = %v (%v)", tree, err)
	}
	e.note(tree.Msg)
	e.noPasswordAnywhere()

	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Errorf("worker stop: %v", err)
	}
}
