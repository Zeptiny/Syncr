package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	controllerconfig "github.com/Zeptiny/Syncr/internal/controller/config"
	"github.com/Zeptiny/Syncr/internal/devseed"
)

// TestJobsSettingsAgree: the configuration's bounds of the jobs settings
// are the job service's.
func TestJobsSettingsAgree(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		config, jobs time.Duration
	}{
		{"shortest time limit", controllerconfig.MinJobTimeLimit, jobs.MinTimeLimit},
		{"longest time limit", controllerconfig.MaxJobTimeLimit, jobs.MaxTimeLimit},
		{"default sample retention", controllerconfig.DefaultSampleRetention, jobs.DefaultSampleRetention},
		{"shortest sample retention", controllerconfig.MinSampleRetention, jobs.MinSampleRetention},
	} {
		if c.config != c.jobs {
			t.Errorf("%s: configuration %v, job service %v", c.name, c.config, c.jobs)
		}
	}
}

// seededJob returns the first job of a ListJobs call.
func seededJob(t *testing.T, jc syncrv1connect.JobServiceClient, req *syncrv1.ListJobsRequest) *syncrv1.Job {
	t.Helper()
	req.AllUsers, req.PageSize = true, 500
	resp, err := jc.ListJobs(t.Context(), connect.NewRequest(req))
	if err != nil || len(resp.Msg.GetJobs()) == 0 {
		t.Fatalf("ListJobs(%v) = %v (%v)", req, resp, err)
	}
	return resp.Msg.GetJobs()[0]
}

// TestRunSeededTimeLimits: jobs.default_max_runtime reaches the specs the
// dispatcher builds and the API's Job.time_limit: a definition's own limit
// wins, a job without one gets the default, a data deletion never does.
// The seeded Milestone 6 objects are served too: the job its limit
// stopped with its thinned samples, the pinned host keys, the canned host
// key probe, the outdated worker with its upgrade commands, and a running
// backup whose size fits its repository.
func TestRunSeededTimeLimits(t *testing.T) {
	t.Parallel()
	s := startSeededWith(t, map[string]string{"SYNCR_JOBS_DEFAULT_MAX_RUNTIME": "2h"})
	ctx := t.Context()
	admin := cookieClient(http.DefaultTransport, s.login(devseed.AdminUsername, devseed.AdminPassword), true)
	jc := syncrv1connect.NewJobServiceClient(admin, s.base)

	limited := seededJob(t, jc, &syncrv1.ListJobsRequest{Query: "raw", Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_FAILED}})
	if limited.GetTimeLimit().AsDuration() != 6*time.Hour || limited.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT ||
		limited.GetDefinition().GetMaxRuntime().AsDuration() != 6*time.Hour {
		t.Errorf("the job its limit stopped = %v", limited)
	}
	samples, err := jc.ListJobSamples(ctx, connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: limited.GetId()}))
	if err != nil || len(samples.Msg.GetSamples()) != jobs.DefaultSamplePoints || samples.Msg.GetTotal() < 4300 {
		t.Errorf("its samples: %d of %d (%v)", len(samples.Msg.GetSamples()), samples.Msg.GetTotal(), err)
	}
	plain := seededJob(t, jc, &syncrv1.ListJobsRequest{Kinds: []string{"rclone_transfer"}, Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_SUCCEEDED}})
	if plain.GetDefinition().GetMaxRuntime() != nil || plain.GetTimeLimit().AsDuration() != 2*time.Hour {
		t.Errorf("a job without a limit = time limit %v, want the default 2h", plain.GetTimeLimit())
	}
	deletion := seededJob(t, jc, &syncrv1.ListJobsRequest{Kinds: []string{"restic_delete_data"}})
	if deletion.GetTimeLimit() != nil {
		t.Errorf("a data deletion gets the time limit %v, want none", deletion.GetTimeLimit())
	}
	backup := seededJob(t, jc, &syncrv1.ListJobsRequest{Kinds: []string{"restic_backup"}, Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_RUNNING}})
	if total := backup.GetStats().GetTotalBytes(); total < 3<<30 || total > 4<<30 || backup.GetStats().GetBytes() >= total {
		t.Errorf("the running backup = %v, want about 3.5 GiB, part way", backup.GetStats())
	}

	// SFTP: one remote pins the demo keys, the probe answers them.
	remotes, err := syncrv1connect.NewRemoteServiceClient(admin, s.base).ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]int{}
	for _, r := range remotes.Msg.GetRemotes() {
		if r.GetBackendType() == "sftp" {
			pinned[r.GetName()] = len(strings.Fields(r.GetOptions()["host_keys"])) / 2
		}
	}
	if pinned["Offsite SSH"] != 2 || pinned["Storage box"] != 0 || len(pinned) != 2 {
		t.Errorf("host keys per SFTP remote = %v", pinned)
	}
	keys, err := syncrv1connect.NewProbeServiceClient(admin, s.base).GetSftpHostKeys(ctx, connect.NewRequest(&syncrv1.GetSftpHostKeysRequest{Host: "backup.example.net"}))
	// The outdated attic-nas cannot fetch keys; an up-to-date worker does.
	if err != nil || len(keys.Msg.GetKeys()) != 2 || keys.Msg.GetWorker().GetName() == "" || keys.Msg.GetWorker().GetName() == "attic-nas" ||
		keys.Msg.GetKeys()[0].GetFingerprintSha256() != "SHA256:dkA939jgBPkIxhIk1YrFbVK6S3sjJRKuDnTe/EF7P54" ||
		keys.Msg.GetKeys()[1].GetFingerprintSha256() != "SHA256:Lu0iomEI1gJTzrNNAEPx/kPI7GZriuysj84Pproxz/I" {
		t.Errorf("GetSftpHostKeys = %v (%v)", keys, err)
	}

	// Workers: attic-nas is the outdated one, and has upgrade commands.
	wc := syncrv1connect.NewWorkerAdminServiceClient(admin, s.base)
	list, err := wc.ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var attic *syncrv1.Worker
	for _, w := range list.Msg.GetWorkers() {
		switch {
		case w.GetName() == "attic-nas":
			attic = w
		case w.GetOutdated():
			t.Errorf("worker %s is outdated", w.GetName())
		}
	}
	if attic == nil || !attic.GetOutdated() || attic.GetStatus() != syncrv1.WorkerStatus_WORKER_STATUS_ONLINE {
		t.Fatalf("attic-nas = %v", attic)
	}
	cmds, err := wc.GetWorkerUpgradeCommands(ctx, connect.NewRequest(&syncrv1.GetWorkerUpgradeCommandsRequest{WorkerId: attic.GetId()}))
	if err != nil || len(cmds.Msg.GetCommands()) != 2 {
		t.Fatalf("GetWorkerUpgradeCommands = %v (%v)", cmds, err)
	}
	// A development build without a mirror: the latest release, which the
	// command picks itself (no --version, no --from).
	for _, c := range cmds.Msg.GetCommands() {
		if cmd := c.GetCommand(); !strings.Contains(cmd, " upgrade") || strings.Contains(cmd, "--version") || strings.Contains(cmd, "--from") {
			t.Errorf("upgrade command of a dev controller = %q", cmd)
		}
	}
}

// TestRunSeededSampleRetention: the hourly purge runs at start with
// jobs.sample_retention: the samples of jobs that finished longer ago are
// gone, those of running jobs stay.
func TestRunSeededSampleRetention(t *testing.T) {
	t.Parallel()
	s := startSeededWith(t, map[string]string{"SYNCR_JOBS_SAMPLE_RETENTION": "1h"})
	waitLog(t, s.logs, `"msg":"progress samples of old jobs purged"`)
	admin := cookieClient(http.DefaultTransport, s.login(devseed.AdminUsername, devseed.AdminPassword), true)
	jc := syncrv1connect.NewJobServiceClient(admin, s.base)
	for _, c := range []struct {
		what string
		req  *syncrv1.ListJobsRequest
		kept bool
	}{
		{"the job its limit stopped a day ago", &syncrv1.ListJobsRequest{Query: "raw"}, false},
		{"a running job", &syncrv1.ListJobsRequest{Kinds: []string{"rclone_transfer"}, Statuses: []syncrv1.JobStatus{syncrv1.JobStatus_JOB_STATUS_RUNNING}}, true},
	} {
		j := seededJob(t, jc, c.req)
		resp, err := jc.ListJobSamples(t.Context(), connect.NewRequest(&syncrv1.ListJobSamplesRequest{JobId: j.GetId()}))
		if err != nil || (resp.Msg.GetTotal() > 0) != c.kept {
			t.Errorf("%s has %d samples (%v), want kept %v", c.what, resp.Msg.GetTotal(), err, c.kept)
		}
	}
}
