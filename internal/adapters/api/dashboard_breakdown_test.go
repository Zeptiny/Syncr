package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestDashboardBreakdowns(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	second := m.addWorker()
	const transfer, backup = "rclone_transfer", "restic_backup"
	m.job(t, testUser, m.worker, transfer, domain.JobSucceeded, "", 2*time.Second, 40*time.Second, 100)
	m.job(t, testUser, m.worker, transfer, domain.JobFailed, "execution_failed", 4*time.Second, 20*time.Second, 5)
	m.job(t, testUser, second, backup, domain.JobFailed, "execution_failed", 0, 0, 0)
	m.job(t, testUser, second, backup, domain.JobInterrupted, "worker_restarted", time.Second, time.Minute, 1)
	m.job(t, testUser, second, backup, domain.JobSucceeded, "", time.Second, time.Minute, 1)
	m.job(t, testUser, second, backup, domain.JobSucceeded, "", time.Second, time.Minute, 1)
	m.job(t, testOther, m.worker, transfer, domain.JobFailed, "not_found", 0, 0, 0) // another user's
	m.clock.Advance(2 * time.Minute)

	msg := m.get(t, testUser, &syncrv1.GetDashboardRequest{})
	tot := msg.GetTotals()
	if tot.GetStarted() != 5 || tot.GetRunTime().AsDuration() != 4*time.Minute || tot.GetDispatched() != 5 || tot.GetQueueWait().AsDuration() != 9*time.Second {
		t.Errorf("totals run %v/%d, wait %v/%d; want 4m/5, 9s/5", tot.GetRunTime().AsDuration(), tot.GetStarted(), tot.GetQueueWait().AsDuration(), tot.GetDispatched())
	}
	var started int64
	for _, b := range msg.GetActivity() {
		started += b.GetStarted()
	}
	if started != 5 {
		t.Errorf("buckets count %d started jobs, want 5", started)
	}

	kinds := msg.GetKinds()
	if len(kinds) != 2 || kinds[0].GetKey() != backup || kinds[1].GetKey() != transfer {
		t.Fatalf("kinds = %v, want backup (4 jobs) then transfer (2)", kinds)
	}
	if k := kinds[0]; k.GetSucceeded() != 2 || k.GetFailed() != 1 || k.GetInterrupted() != 1 || k.GetStarted() != 3 || k.GetRunTime().AsDuration() != 3*time.Minute {
		t.Errorf("backup = %v", k)
	}
	if k := kinds[1]; k.GetSucceeded() != 1 || k.GetFailed() != 1 || k.GetBytes() != 105 || k.GetQueueWait().AsDuration() != 6*time.Second {
		t.Errorf("transfer = %v", k)
	}
	ws := msg.GetWorkerActivity()
	if len(ws) != 2 || ws[0].GetKey() != second.String() || ws[0].GetName() != "worker-"+second.String() || ws[1].GetName() != "worker" {
		t.Fatalf("workers = %v", ws)
	}

	reasons := msg.GetFailureReasons()
	if len(reasons) != 2 {
		t.Fatalf("failure reasons = %v", reasons)
	}
	if r := reasons[0]; r.GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || r.GetFailed() != 2 || r.GetInterrupted() != 0 ||
		!slices.Equal(r.GetKinds(), []string{transfer, backup}) { // one each: by name
		t.Errorf("first reason = %v", r)
	}
	if r := reasons[1]; r.GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_RESTARTED || r.GetInterrupted() != 1 {
		t.Errorf("second reason = %v", r)
	}

	// An admin with all users also counts the other user's failure; ties
	// go by code.
	all := m.get(t, testAdmin, &syncrv1.GetDashboardRequest{AllUsers: true})
	if rs := all.GetFailureReasons(); len(rs) != 3 || rs[1].GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND ||
		rs[2].GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_RESTARTED {
		t.Errorf("admin's failure reasons = %v", rs)
	}
}

func TestDashboardScheduleHealth(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	ok := schedules.Run{Status: schedules.RunLaunched, JobStatus: domain.JobSucceeded}
	m.sched.health = []schedules.Health{
		{Schedule: schedules.Schedule{ID: domain.NewScheduleID(), Name: "A ok", Enabled: true}, HasRun: true, LatestRun: ok, LastSuccessAt: time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)},
		{Schedule: schedules.Schedule{ID: domain.NewScheduleID(), Name: "B never ran", Enabled: true}},
		{Schedule: schedules.Schedule{ID: domain.NewScheduleID(), Name: "C missed"}, HasRun: true, LatestRun: schedules.Run{Status: schedules.RunMissed}},
		{Schedule: schedules.Schedule{ID: domain.NewScheduleID(), Name: "D running"}, HasRun: true, LatestRun: schedules.Run{Status: schedules.RunLaunched, JobStatus: domain.JobRunning}},
		{Schedule: schedules.Schedule{ID: domain.NewScheduleID(), Name: "E failed"}, HasRun: true, LatestRun: schedules.Run{Status: schedules.RunLaunched, JobStatus: domain.JobFailed}},
	}
	msg := m.get(t, testUser, &syncrv1.GetDashboardRequest{})
	var names []string
	for _, s := range msg.GetSchedules() {
		names = append(names, s.GetScheduleName())
	}
	if want := []string{"C missed", "E failed", "A ok", "B never ran", "D running"}; !slices.Equal(names, want) {
		t.Errorf("schedules = %v, want %v (problems first)", names, want)
	}
	if s := msg.GetSchedules()[2]; s.GetLatestRun().GetJobStatus() != syncrv1.JobStatus_JOB_STATUS_SUCCEEDED || s.GetLastSuccessAt() == nil {
		t.Errorf("A ok = %v", s)
	}
	if s := msg.GetSchedules()[3]; s.GetLatestRun() != nil || s.GetLastSuccessAt() != nil {
		t.Errorf("B never ran = %v", s)
	}
}

func TestDashboardRepositoryGrowth(t *testing.T) {
	t.Parallel()
	m := newMetricsEnv(t)
	m.h.restic = m.repos
	m.dashEnv.h.restic = m.repos
	photos, docs, gone := domain.NewResticRepoID(), domain.NewResticRepoID(), domain.NewResticRepoID()
	ctx := context.Background()
	remote := remotes.Remote{ID: domain.NewRemoteID(), OwnerID: testUser.UserID, Name: "b2", Type: "s3", Version: 1}
	if err := m.store.CreateRemote(ctx, remote); err != nil {
		t.Fatal(err)
	}
	for i, id := range []domain.ResticRepoID{photos, docs, gone} {
		path := []string{"photos", "docs", "gone"}[i]
		r := restic.Repo{ID: id, OwnerID: testUser.UserID, Name: path, RemoteID: remote.ID, Path: path,
			PasswordEnc: []byte("enc"), ResticID: strings.Repeat(string(rune('a'+i)), 64), FormatVersion: 2, Version: 1}
		if err := m.store.CreateRepo(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	m.repos.repos = []restic.Repo{
		{ID: photos, Name: "photos", Stats: restic.RepoStats{SnapshotCount: 9, LatestSnapshotAt: time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC)}},
		{ID: docs, Name: "Docs"},
		{ID: gone, Name: "Gone", DeletedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}
	now := m.clock.Now() // 12:00
	backupJob := func(owner domain.Actor, repo domain.ResticRepoID, st domain.JobStatus, finished time.Time, packed, plain, source int64) {
		t.Helper()
		out, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
			DataAddedPackedBytes: packed, DataAddedBytes: plain, TotalBytesProcessed: source,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		j := jobs.Job{
			ID: domain.NewJobID(), WorkerID: m.worker, OwnerID: owner.UserID, Kind: restic.KindBackup, Status: st,
			Definition: []byte(`{}`), RepositoryID: repo, CreatedAt: finished.Add(-time.Minute), QueuedAt: finished.Add(-time.Minute),
			StartedAt: finished.Add(-time.Minute), FinishedAt: finished, Output: out, ExitCode: 0, Version: 1,
		}
		if err := m.store.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	backupJob(testUser, photos, domain.JobSucceeded, now.Add(-20*time.Hour), 100, 300, 1000)
	backupJob(testUser, photos, domain.JobSucceeded, now.Add(-2*time.Hour), 40, 90, 1100)
	backupJob(testUser, photos, domain.JobSucceeded, now.Add(-30*time.Minute), 0, 7, 1200) // no packed size: the plain one counts
	backupJob(testUser, photos, domain.JobFailed, now.Add(-time.Hour), 999, 999, 999)
	backupJob(testUser, photos, domain.JobSucceeded, now.Add(-30*time.Hour), 999, 999, 999) // before the range
	backupJob(testUser, gone, domain.JobSucceeded, now.Add(-time.Hour), 999, 999, 999)
	backupJob(testOther, docs, domain.JobSucceeded, now.Add(-time.Hour), 999, 999, 999) // not the user's job

	msg := m.get(t, testUser, &syncrv1.GetDashboardRequest{})
	repos := msg.GetRepositories()
	if len(repos) != 2 || repos[0].GetName() != "Docs" || repos[1].GetName() != "photos" {
		t.Fatalf("repositories = %v, want Docs and photos by name", repos)
	}
	if d := repos[0]; d.GetBackups() != 0 || d.GetAddedBytes() != 0 || len(d.GetAdded()) != len(msg.GetActivity()) {
		t.Errorf("Docs = %v", d)
	}
	p := repos[1]
	if p.GetSnapshotCount() != 9 || p.GetBackups() != 3 || p.GetAddedBytes() != 147 || p.GetFirstSourceBytes() != 1000 ||
		p.GetLastSourceBytes() != 1200 || p.GetTruncated() {
		t.Errorf("photos = %v", p)
	}
	added := p.GetAdded()
	// The range starts at 13:00 yesterday: at 12:00 sharp it has 23 hours.
	if len(added) != len(msg.GetActivity()) || len(added) != 23 || added[3] != 100 || added[21] != 40 || added[22] != 7 {
		t.Errorf("photos per bucket = %v, want 100 at 3, 40 at 21, 7 at 22", added)
	}
}
