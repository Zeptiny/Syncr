package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/devseed"
)

// TestRunSeededRestic: on a seeded controller the restic and probe
// services are wired through to the canned probes: repositories with
// their snapshots, a refresh, a snapshot's tree, a worker's folders, a
// remote test, a new repository, and the jobs that wait for a repository.
// It also checks that another user cannot reach any of it and that the
// demo repositories' password is in no response and no log line.
func TestRunSeededRestic(t *testing.T) {
	t.Parallel()
	s := startSeeded(t)
	ctx := t.Context()
	admin := cookieClient(http.DefaultTransport, s.login(devseed.AdminUsername, devseed.AdminPassword), true)
	alice := cookieClient(http.DefaultTransport, s.login(devseed.AliceUsername, devseed.AlicePassword), true)
	rc := syncrv1connect.NewResticServiceClient(admin, s.base)
	pc := syncrv1connect.NewProbeServiceClient(admin, s.base)
	jc := syncrv1connect.NewJobServiceClient(admin, s.base)
	var seen []string // every response, to search for the password

	list, err := rc.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, list.Msg.String())
	repos := map[string]*syncrv1.Repository{}
	for _, r := range list.Msg.GetRepositories() {
		repos[r.GetName()] = r
	}
	for name, snapshots := range map[string]int32{"Photos vault": 30, "NAS documents": 14, "Office laptop": 10, "Old laptop": 6} {
		r := repos[name]
		if r == nil || r.GetStats().GetSnapshotCount() != snapshots || r.GetRemote().GetName() == "" || r.GetStats().GetLastBackup() == nil {
			t.Errorf("repository %q = %v", name, r)
		}
	}
	if len(repos) != 4 {
		t.Errorf("the admin's repositories = %v, want 4 (alice's and the deleted one are not listed)", len(repos))
	}
	photos, old := repos["Photos vault"], repos["Old laptop"]
	if photos == nil || old == nil {
		t.FailNow()
	}
	if photos.GetRemote().GetName() != "B2 offsite" || photos.GetPath() != "restic/photos" || photos.GetStats().GetScheduleCount() != 3 {
		t.Errorf("Photos vault = %v", photos)
	}

	// The snapshot cache, then a refresh through the (canned) worker.
	cached, err := rc.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: photos.GetId()}))
	if err != nil || len(cached.Msg.GetSnapshots()) != 30 || cached.Msg.GetWorker() != nil {
		t.Fatalf("ListSnapshots = %v (%v)", cached, err)
	}
	fresh, err := rc.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: photos.GetId(), Refresh: true}))
	if err != nil || len(fresh.Msg.GetSnapshots()) != 30 || fresh.Msg.GetRefreshError() != "" || fresh.Msg.GetStale() ||
		fresh.Msg.GetWorker().GetName() == "" || fresh.Msg.GetSnapshots()[0].GetId() != cached.Msg.GetSnapshots()[0].GetId() {
		t.Fatalf("ListSnapshots(refresh) = %v (%v)", fresh, err)
	}
	seen = append(seen, fresh.Msg.String())
	snapshot := fresh.Msg.GetSnapshots()[0]
	if !slices.Equal(snapshot.GetPaths(), []string{"/srv/photos"}) || snapshot.GetSummary().GetTotalBytesProcessed() == 0 {
		t.Errorf("newest snapshot = %v", snapshot)
	}
	tree, err := rc.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: photos.GetId(), SnapshotId: snapshot.GetId(), Path: "/srv/photos/2026"}))
	if err != nil || len(tree.Msg.GetEntries()) != 12 || tree.Msg.GetEntries()[0].GetName() != "01" {
		t.Fatalf("BrowseSnapshot = %v (%v)", tree, err)
	}
	seen = append(seen, tree.Msg.String())
	if _, err := rc.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: photos.GetId(), SnapshotId: snapshot.GetId(), Path: "/nope"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("BrowseSnapshot(missing folder) = %v", err)
	}

	// A worker's folders and a remote's, and remote tests.
	workers, err := syncrv1connect.NewWorkerAdminServiceClient(admin, s.base).ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, w := range workers.Msg.GetWorkers() {
		ids[w.GetName()] = w.GetId()
		// attic-nas runs probes too; it only lacks the time limits.
		if up := w.GetName() != "laptop"; up != slices.Contains(w.GetInfo().GetFeatures(), "probe") {
			t.Errorf("worker %s features = %v", w.GetName(), w.GetInfo().GetFeatures())
		}
	}
	roots, err := pc.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: ids["nas-01"]}))
	if err != nil || len(roots.Msg.GetEntries()) != 2 || roots.Msg.GetEntries()[0].GetPath() != "/srv" || roots.Msg.GetWorker().GetName() != "nas-01" {
		t.Fatalf("ListWorkerFiles(roots) = %v (%v)", roots, err)
	}
	if _, err := pc.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: ids["nas-01"], Path: "/etc"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("ListWorkerFiles(outside the roots) = %v", err)
	}
	// The office PC is offline: nothing can be listed there.
	if _, err := pc.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: ids["office-pc"]})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("ListWorkerFiles(offline worker) = %v", err)
	}
	remotes, err := syncrv1connect.NewRemoteServiceClient(admin, s.base).ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	remoteIDs := map[string]string{}
	for _, r := range remotes.Msg.GetRemotes() {
		remoteIDs[r.GetName()] = r.GetId()
	}
	files, err := pc.ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: remoteIDs["MinIO archive"], Path: "restic"}))
	if err != nil || len(files.Msg.GetEntries()) != 1 || files.Msg.GetEntries()[0].GetPath() != "restic/nas-docs" {
		t.Fatalf("ListRemoteFiles = %v (%v)", files, err)
	}
	seen = append(seen, files.Msg.String())
	for name, want := range map[string]func(*syncrv1.TestRemoteResponse) bool{
		"B2 offsite": func(r *syncrv1.TestRemoteResponse) bool {
			return r.GetOk() && r.GetUsage().GetUsed() > 0 && len(r.GetSample()) > 0
		},
		"MinIO archive": func(r *syncrv1.TestRemoteResponse) bool { return r.GetOk() && r.GetUsage().GetUnsupported() },
		"Nextcloud": func(r *syncrv1.TestRemoteResponse) bool {
			return !r.GetOk() && strings.Contains(r.GetError(), "connection refused") && r.GetWorker().GetName() != ""
		},
	} {
		res, err := pc.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{RemoteId: remoteIDs[name]}))
		if err != nil || !want(res.Msg) {
			t.Errorf("TestRemote(%s) = %v (%v)", name, res, err)
			continue
		}
		seen = append(seen, res.Msg.String())
	}

	// A new repository: created where none is, refused where one is.
	const password = "a-new-demo-password"
	input := func(name, path string, mode syncrv1.RepositoryCreateMode) *connect.Request[syncrv1.CreateRepositoryRequest] {
		return connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: &syncrv1.RepositoryInput{
			Name: name, RemoteId: remoteIDs["MinIO archive"], Path: path, Password: password, Mode: mode,
		}})
	}
	created, err := rc.CreateRepository(ctx, input("Scratch", "restic/scratch", syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT))
	if err != nil || len(created.Msg.GetRepository().GetResticId()) != 64 || created.Msg.GetWorker().GetName() == "" {
		t.Fatalf("CreateRepository = %v (%v)", created, err)
	}
	seen = append(seen, created.Msg.String())
	if _, err := rc.CreateRepository(ctx, input("Again", "restic/nas-docs", syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT)); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("CreateRepository(over NAS documents) = %v", err)
	}
	if _, err := rc.CreateRepository(ctx, input("Nothing", "restic/nothing", syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_CONNECT)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("CreateRepository(connect to nothing) = %v", err)
	}
	if _, err := rc.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: created.Msg.GetRepository().GetId()})); err != nil {
		t.Errorf("DeleteRepository(forget only) = %v", err)
	}

	// The repository whose data is being deleted, and the waiting jobs.
	if !old.GetDeleting() || old.GetDeletionJobId() == "" || old.GetDeletionOutcome() != syncrv1.RepositoryDeletionOutcome_REPOSITORY_DELETION_OUTCOME_UNSPECIFIED {
		t.Fatalf("Old laptop = %v", old)
	}
	del, err := jc.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: old.GetDeletionJobId()}))
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, del.Msg.String())
	dj := del.Msg.GetJob()
	if dj.GetKind() != "restic_delete_data" || dj.GetStatus() != syncrv1.JobStatus_JOB_STATUS_QUEUED || dj.GetWaitingForJobId() == "" ||
		dj.GetRepository().GetName() != "Old laptop" || !strings.Contains(dj.GetTitle(), "Old laptop") {
		t.Errorf("the deletion job = %v", dj)
	}
	blocker, err := jc.GetJob(ctx, connect.NewRequest(&syncrv1.GetJobRequest{JobId: dj.GetWaitingForJobId()}))
	if err != nil || blocker.Msg.GetJob().GetKind() != "restic_check" || blocker.Msg.GetJob().GetStatus() != syncrv1.JobStatus_JOB_STATUS_RUNNING {
		t.Errorf("the deletion waits for %v (%v)", blocker, err)
	}
	for what, call := range map[string]func() error{
		"a job on the deleting repository": func() error {
			_, err := jc.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: ids["nas-01"], Definition: &syncrv1.JobDefinition{
				Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{RepositoryId: old.GetId()}},
			}}))
			return err
		},
		"a refresh of the deleting repository": func() error {
			_, err := rc.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: old.GetId(), Refresh: true}))
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s = %v, want FailedPrecondition", what, err)
		}
	}
	if _, err := jc.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: ids["nas-01"], Definition: &syncrv1.JobDefinition{
		Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: photos.GetId()}},
	}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("RunJob(restic_delete_data) = %v, want InvalidArgument: only deleting a repository starts it", err)
	}
	jobs, err := jc.ListJobs(ctx, connect.NewRequest(&syncrv1.ListJobsRequest{RepositoryId: repos["NAS documents"].GetId(), PageSize: 100}))
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, jobs.Msg.String())
	var waiting, running, detailed int
	for _, j := range jobs.Msg.GetJobs() {
		switch {
		case j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_QUEUED && j.GetWaitingForJobId() != "":
			waiting++
		case j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_RUNNING:
			running++
		case j.GetStatus() == syncrv1.JobStatus_JOB_STATUS_SUCCEEDED && j.GetKind() == "restic_backup" && len(j.GetDetails()) > 0:
			detailed++
		}
		if j.GetRepository().GetName() != "NAS documents" || j.GetTitle() == "" {
			t.Errorf("job %s: repository %v, title %q", j.GetId(), j.GetRepository(), j.GetTitle())
		}
	}
	if waiting != 1 || running != 1 || detailed < 3 {
		t.Errorf("NAS documents: %d waiting, %d running, %d backups with details", waiting, running, detailed)
	}

	// Another user reaches none of it, and neither does anyone unknown.
	arc := syncrv1connect.NewResticServiceClient(alice, s.base)
	for what, call := range map[string]func() error{
		"GetRepository": func() error {
			_, err := arc.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: photos.GetId()}))
			return err
		},
		"ListSnapshots": func() error {
			_, err := arc.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: photos.GetId()}))
			return err
		},
		"BrowseSnapshot": func() error {
			_, err := arc.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: photos.GetId(), SnapshotId: snapshot.GetId()}))
			return err
		},
		"DeleteRepository with its data": func() error {
			_, err := arc.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: photos.GetId(), DeleteData: true, ConfirmName: "Photos vault"}))
			return err
		},
		"ListRemoteFiles": func() error {
			_, err := syncrv1connect.NewProbeServiceClient(alice, s.base).ListRemoteFiles(ctx, connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: remoteIDs["MinIO archive"]}))
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("alice: %s = %v, want NotFound", what, err)
		}
	}
	if mine, err := arc.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{})); err != nil || len(mine.Msg.GetRepositories()) != 1 {
		t.Errorf("alice's repositories = %v (%v)", mine, err)
	}
	anon := syncrv1connect.NewResticServiceClient(http.DefaultClient, s.base)
	if _, err := anon.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ListRepositories without a session = %v", err)
	}
	if still, err := rc.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: photos.GetId()})); err != nil || still.Msg.GetRepository().GetDeleting() {
		t.Errorf("Photos vault after alice's attempts = %v (%v)", still, err)
	}

	// Deleting the data needs the repository's exact name, and the
	// deletion kind cannot be scheduled either.
	for _, confirm := range []string{"", "photos vault", "Photos vault "} {
		if _, err := rc.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: photos.GetId(), DeleteData: true, ConfirmName: confirm})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("DeleteRepository with its data, confirmed with %q = %v, want InvalidArgument", confirm, err)
		}
	}
	if _, err := syncrv1connect.NewScheduleServiceClient(admin, s.base).CreateSchedule(ctx, connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: &syncrv1.ScheduleInput{
		Name: "Delete it nightly", Cron: "0 3 * * *", Timezone: "UTC", Enabled: true,
		Target:    &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: ids["nas-01"]}},
		MissedRun: &syncrv1.MissedRunPolicy{Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_SKIP},
		Overlap:   syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP,
		Definition: &syncrv1.JobDefinition{
			Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: photos.GetId()}},
		},
	}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("CreateSchedule(restic_delete_data) = %v, want InvalidArgument", err)
	}
	if still, err := rc.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: photos.GetId()})); err != nil || still.Msg.GetRepository().GetDeleting() {
		t.Errorf("Photos vault after the refused deletions = %v (%v)", still, err)
	}

	// Probes are rate-limited per user: alice's flood is cut off, and the
	// admin can still probe.
	apc := syncrv1connect.NewProbeServiceClient(alice, s.base)
	limited := false
	for range 40 {
		if _, err := apc.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: ids["nas-01"]})); connect.CodeOf(err) == connect.CodeResourceExhausted {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("40 probes in a row were all allowed, want ResourceExhausted")
	}
	if _, err := pc.ListWorkerFiles(ctx, connect.NewRequest(&syncrv1.ListWorkerFilesRequest{WorkerId: ids["nas-01"]})); err != nil {
		t.Errorf("the admin's probe after alice's flood = %v", err)
	}

	// Passwords: in no response and no log line.
	for _, secret := range []string{"syncr-demo-restic", password} {
		for _, text := range seen {
			if strings.Contains(text, secret) {
				t.Errorf("a response contains the repository password %q: %s", secret, text)
			}
		}
		if strings.Contains(s.logs.String(), secret) {
			t.Errorf("the log contains the repository password %q", secret)
		}
	}
}
