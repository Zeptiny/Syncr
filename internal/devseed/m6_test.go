package devseed_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// The demo SSH servers' host keys and their fingerprints, as the
// screenshots of the SFTP form show them.
var demoKeys = []struct{ line, fingerprint string }{
	{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMUiJpUbcNynH9rHhq4iK4jdXA7Qg/xNVqitUwiNweax", "SHA256:dkA939jgBPkIxhIk1YrFbVK6S3sjJRKuDnTe/EF7P54"},
	{"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBH9xAeBCuXqzBfh9cf86Gd6fiPvC0DUhn7/y6t//sQHCSwUg+aU5NxZuU/ADvvNXRTI5S3AoUmxGN7kbZLiU6XQ=",
		"SHA256:Lu0iomEI1gJTzrNNAEPx/kPI7GZriuysj84Pproxz/I"},
}

// TestSeedMilestone6: the demo objects of Milestone 6's screens. A
// transfer stopped by its 6 hour time limit, with about 4,320 samples; an
// SFTP remote with pinned host keys next to one without; an online worker
// that lacks a link feature; the canned host keys of every SSH server; and
// a running backup whose size fits its repository.
func TestSeedMilestone6(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	// The first failed job that matches "raw" is the one its time limit
	// stopped.
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{Query: "raw", Statuses: []domain.JobStatus{domain.JobFailed}})
	if err != nil || len(page.Jobs) == 0 {
		t.Fatalf("failed jobs matching raw = %v (%v)", page.Jobs, err)
	}
	limited := page.Jobs[0]
	if limited.Error.Code != jobs.CodeTimeLimit || limited.Error.Message != "stopped after 6h0m0s: the job's time limit was reached" ||
		limited.OwnerID != s.Admin.UserID || limited.WorkerID != s.Workers["nas-01"] || limited.Kind != "rclone_transfer" {
		t.Errorf("time-limited job = %+v", limited)
	}
	if run := limited.FinishedAt.Sub(limited.StartedAt); run != 6*time.Hour {
		t.Errorf("it ran %v, want its 6h limit", run)
	}
	if limited.TimeLimit != 6*time.Hour {
		t.Errorf("it is recorded as dispatched with the limit %v, want 6h", limited.TimeLimit)
	}
	def, err := jobkind.Unmarshal(limited.Definition)
	if err != nil || def.GetMaxRuntime().AsDuration() != 6*time.Hour {
		t.Fatalf("its definition = %v (%v), want max_runtime 6h", def, err)
	}
	tr := def.GetRcloneTransfer()
	if tr.GetSource().GetRemoteId() != s.Remotes["Photos"].String() || tr.GetSource().GetPath() != "raw" ||
		tr.GetDestination().GetRemoteId() != s.Remotes["B2 offsite"].String() || tr.GetDestination().GetPath() != "photos-raw" {
		t.Errorf("its transfer = %v", tr)
	}
	thinned, total, err := e.deps.Jobs.ListSamples(ctx, s.Admin, limited.ID, 0)
	if err != nil || total < 4300 || total > 4320 || len(thinned) != jobs.DefaultSamplePoints {
		t.Errorf("its samples: %d of %d (%v), want 1000 of about 4,320", len(thinned), total, err)
	}

	// Remotes: Storage box is not verified, Offsite SSH pins both keys.
	storage, err := e.deps.Remotes.Get(ctx, s.Admin, s.Remotes["Storage box"])
	if err != nil || storage.Type != "sftp" || storage.Options["host_keys"] != "" {
		t.Errorf("Storage box = %+v (%v)", storage, err)
	}
	offsite, err := e.deps.Remotes.Get(ctx, s.Admin, s.Remotes["Offsite SSH"])
	wantKeys := demoKeys[0].line + "\n" + demoKeys[1].line
	if err != nil || offsite.Type != "sftp" || offsite.Options["host"] != "backup.example.net" || offsite.Options["host_keys"] != wantKeys {
		t.Errorf("Offsite SSH = %+v (%v)", offsite, err)
	}

	// attic-nas is online, reported its Hello, and lacks the time limits.
	attic, err := e.deps.Workers.Get(ctx, s.Workers["attic-nas"])
	if err != nil || e.deps.Workers.Status(attic) != domain.WorkerOnline || attic.Removed() || attic.Info.ReportedAt.IsZero() ||
		attic.Info.OS != "linux" || attic.Info.Arch != "amd64" {
		t.Fatalf("attic-nas = %+v (%v)", attic, err)
	}
	if slices.Contains(attic.Info.Features, link.FeatureJobTimeLimit) || !slices.Contains(attic.Info.Features, link.FeatureProbe) {
		t.Errorf("attic-nas features = %v, want all but %s", attic.Info.Features, link.FeatureJobTimeLimit)
	}
	if slices.Contains(attic.Info.ProbeOps, "sftp_host_keys") || !slices.Contains(attic.Info.ProbeOps, "list_local") {
		t.Errorf("attic-nas probe operations = %v, want those before Milestone 6", attic.Info.ProbeOps)
	}

	// Every SSH server offers the demo keys.
	res, err := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "keys", Spec: &syncrv1.ProbeSpec{
		Op: &syncrv1.ProbeSpec_SftpHostKeys{SftpHostKeys: &syncrv1.ProbeSftpHostKeys{Host: "backup.example.net", Port: 22}},
	}})
	if err != nil || res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
		t.Fatalf("sftp_host_keys = %v (%v)", res, err)
	}
	keys := res.GetSftpHostKeys().GetKeys()
	if len(keys) != len(demoKeys) {
		t.Fatalf("host keys = %v", keys)
	}
	for i, k := range keys {
		if got := k.GetAlgorithm() + " " + k.GetKey(); got != demoKeys[i].line || k.GetFingerprintSha256() != demoKeys[i].fingerprint {
			t.Errorf("host key %d = %v, want %s (%s)", i, k, demoKeys[i].line, demoKeys[i].fingerprint)
		}
	}
	if res, _ := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "bad", Spec: &syncrv1.ProbeSpec{
		Op: &syncrv1.ProbeSpec_SftpHostKeys{SftpHostKeys: &syncrv1.ProbeSftpHostKeys{}},
	}}); res.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC {
		t.Errorf("sftp_host_keys without a host = %v", res)
	}

	// The running backup of the NAS documents reads about what the
	// earlier backups did, and is part way through.
	docs, err := e.store.GetRepo(ctx, s.Repositories["NAS documents"])
	if err != nil {
		t.Fatal(err)
	}
	list, err := e.store.ListSnapshots(ctx, docs.ID)
	if err != nil || len(list.Snapshots) == 0 {
		t.Fatalf("NAS documents snapshots = %v (%v)", list, err)
	}
	read := list.Snapshots[0].Summary.TotalBytesProcessed
	running := runningOn(t, e, s.Admin, docs)
	samples, _, err := e.deps.Jobs.ListSamples(ctx, s.Admin, running.ID, 0)
	if err != nil || len(samples) < 10 {
		t.Fatalf("the running backup has %d samples (%v)", len(samples), err)
	}
	last := samples[len(samples)-1].Stats
	if last.TotalBytes < read || last.TotalBytes > read+read/5 || last.Bytes < last.TotalBytes/4 || last.Bytes > last.TotalBytes*9/10 {
		t.Errorf("the running backup is at %d of %d bytes; the repository's backups read %d", last.Bytes, last.TotalBytes, read)
	}
	if strings.Contains(e.logs.String(), "syncr-demo-offsite-password") {
		t.Error("the log contains the Offsite SSH password")
	}
}

// runningOn returns the running job on repo.
func runningOn(t *testing.T, e *env, actor domain.Actor, repo restic.Repo) jobs.Job {
	t.Helper()
	page, err := e.deps.Jobs.List(context.Background(), actor, jobs.ListFilter{RepositoryID: repo.ID, Statuses: []domain.JobStatus{domain.JobRunning}})
	if err != nil || len(page.Jobs) != 1 {
		t.Fatalf("running jobs on %s = %v (%v)", repo.Name, page.Jobs, err)
	}
	return page.Jobs[0]
}
