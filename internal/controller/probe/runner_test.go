package probe_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

func configResult(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return &syncrv1.ProbeResult{
		ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK,
		Output: &syncrv1.ProbeResult_RepositoryConfig{RepositoryConfig: &syncrv1.ResticRepositoryConfig{Id: strings.Repeat("ab", 32), Version: 2}},
	}, nil
}

func TestVerify(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = configResult
	remote := e.remotes.s3("bucket")
	busy := e.workers.add("busy", domain.WorkerOnline, current())
	idle := e.workers.add("idle", domain.WorkerOnline, current())
	e.workers.add("old", domain.WorkerOnline, workers.Info{Version: "v2.4.0"})
	e.jobs.unfinished[busy] = 3
	ctx := context.Background()
	target := restic.Target{RemoteID: remote, Path: "backups/photos", Password: repoPassword}

	// A new repository: init on the least busy worker.
	got, err := e.runner.Verify(ctx, alice.UserID, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ResticID != strings.Repeat("ab", 32) || got.FormatVersion != 2 || got.WorkerID != idle || got.WorkerName != "idle" {
		t.Errorf("result = %+v", got)
	}
	sent := e.transport.last(t)
	repo := sent.probe.GetSpec().GetResticInit().GetRepository()
	if sent.worker != idle || repo.GetPassword() != repoPassword || repo.GetLocation().GetPath() != "bucket/backups/photos" ||
		repo.GetLocation().GetRemote() == "" || sent.probe.GetTimeout().AsDuration() != 45*time.Second {
		t.Errorf("sent to %v: location %v, timeout %v", sent.worker, repo.GetLocation(), sent.probe.GetTimeout().AsDuration())
	}
	if defs := sent.probe.GetSpec().GetRemotes(); len(defs) != 1 || defs[0].GetSecrets()["secret_access_key"] != remoteSecret ||
		defs[0].GetAlias() != repo.GetLocation().GetRemote() {
		t.Errorf("remotes = %d", len(defs))
	}
	if _, err := domain.ParseProbeID(sent.probe.GetProbeId()); err != nil {
		t.Errorf("probe id %q: %v", sent.probe.GetProbeId(), err)
	}

	// A stored repository: cat config, on the worker of its newest
	// succeeded job although it is busier.
	target.RepositoryID = domain.NewResticRepoID()
	e.jobs.lastByRepo[target.RepositoryID] = busy
	if got, err = e.runner.Verify(ctx, alice.UserID, target, false); err != nil || got.WorkerID != busy {
		t.Fatalf("connect = %+v, %v; want the preferred worker", got, err)
	}
	if sent = e.transport.last(t); sent.probe.GetSpec().GetResticConfig() == nil {
		t.Errorf("connect sent %v", e.transport.ops())
	}

	// An explicit worker wins over the pick.
	target.WorkerID = idle
	if got, err = e.runner.Verify(ctx, alice.UserID, target, false); err != nil || got.WorkerID != idle {
		t.Fatalf("explicit = %+v, %v", got, err)
	}
	e.noSecrets()
}

func TestVerifyRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = configResult
	remote, disk := e.remotes.s3("bucket"), e.remotes.local("/srv/backups")
	online := e.workers.add("nas", domain.WorkerOnline, current())
	offline := e.workers.add("away", domain.WorkerOffline, current())
	old := e.workers.add("old", domain.WorkerOnline, workers.Info{Version: "v2.4.0"})
	ctx := context.Background()

	tests := []struct {
		name   string
		owner  domain.UserID
		target restic.Target
		want   error
		field  string
		msg    string
	}{
		{"another user's remote", bob.UserID, restic.Target{RemoteID: remote}, domain.ErrNotFound, "", ""},
		{"an unknown remote", alice.UserID, restic.Target{RemoteID: domain.NewRemoteID()}, domain.ErrNotFound, "", ""},
		{"no remote", alice.UserID, restic.Target{}, domain.ErrNotFound, "", ""},
		{"a path with ..", alice.UserID, restic.Target{RemoteID: remote, Path: "../other"}, domain.ErrInvalidArgument, "path", ""},
		{"a local remote without a worker", alice.UserID, restic.Target{RemoteID: disk, Path: "repo"}, domain.ErrInvalidArgument, "worker_id", "choose the worker whose disk holds this repository"},
		{"an unknown worker", alice.UserID, restic.Target{RemoteID: remote, WorkerID: domain.NewWorkerID()}, domain.ErrNotFound, "", ""},
		{"an offline worker", alice.UserID, restic.Target{RemoteID: remote, WorkerID: offline}, domain.ErrUnavailable, "", `worker "away" is offline`},
		{"an outdated worker", alice.UserID, restic.Target{RemoteID: remote, WorkerID: old}, domain.ErrFailedPrecondition, "", `worker "old" (version v2.4.0) cannot run this request; upgrade it`},
	}
	for _, tt := range tests {
		tt.target.Password = repoPassword
		before := e.transport.count()
		_, err := e.runner.Verify(ctx, tt.owner, tt.target, false)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: err = %v, want %v with %q", tt.name, err, tt.want, tt.msg)
		}
		var verr *domain.ValidationError
		if tt.field != "" && (!errors.As(err, &verr) || verr.Fields[0].Field != tt.field) {
			t.Errorf("%s: err = %v, want a validation error on %q", tt.name, err, tt.field)
		}
		if e.transport.count() != before {
			t.Errorf("%s: a probe was sent", tt.name)
		}
		e.noSecrets(err.Error())
	}

	// On the chosen worker a local repository is a local path.
	if _, err := e.runner.Verify(ctx, alice.UserID, restic.Target{RemoteID: disk, Path: "repo", WorkerID: online, Password: repoPassword}, false); err != nil {
		t.Fatal(err)
	}
	sent := e.transport.last(t)
	if loc := sent.probe.GetSpec().GetResticConfig().GetRepository().GetLocation(); loc.GetRemote() != "" || loc.GetPath() != "/srv/backups/repo" ||
		len(sent.probe.GetSpec().GetRemotes()) != 0 {
		t.Errorf("local repository location = %v", loc)
	}

	// No worker at all can run it.
	none := newEnv(t)
	none.workers.add("old", domain.WorkerOnline, workers.Info{})
	if _, err := none.runner.Verify(ctx, alice.UserID, restic.Target{RemoteID: none.remotes.s3("b")}, false); !errors.Is(err, probe.ErrNoWorker) {
		t.Errorf("no usable worker: %v", err)
	}

	// The rate limit comes first and is keyed by the user.
	e.limiter.close()
	before := e.transport.count()
	if _, err := e.runner.Verify(ctx, alice.UserID, restic.Target{RemoteID: remote}, false); !errors.Is(err, domain.ErrRateLimited) || e.transport.count() != before {
		t.Errorf("rate limited: %v", err)
	}
	if last := e.limiter.keys[len(e.limiter.keys)-1]; last != alice.UserID.String() {
		t.Errorf("limiter key = %q", last)
	}
	if _, err := e.runner.Verify(ctx, domain.UserID{}, restic.Target{RemoteID: remote}, false); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("no user: %v", err)
	}
}

// TestFailureMapping is the error table of the M5 design (§6.7).
func TestFailureMapping(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	remote := e.remotes.s3("bucket")
	e.workers.add("nas", domain.WorkerOnline, current())
	ctx := context.Background()
	target := restic.Target{RemoteID: remote, Password: repoPassword}
	long := strings.Repeat("é", 600)

	tests := []struct {
		name   string
		answer func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error)
		want   error
		field  string
		msg    string
		code   string // of the *restic.ProbeError; "" if none
	}{
		{"path not allowed", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "outside"), domain.ErrInvalidArgument,
			"path", "is outside the worker's allowed folders", "path_not_allowed"},
		{"not found", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "no such folder"), domain.ErrInvalidArgument,
			"path", "does not exist", "not_found"},
		{"wrong password", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, "wrong password"), domain.ErrFailedPrecondition,
			"", "the stored password no longer opens this repository", "wrong_password"},
		{"no repository", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND, "no config"), domain.ErrFailedPrecondition,
			"", "no repository at the stored location", "repository_not_found"},
		{"timeout", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT, "too slow"), domain.ErrUnavailable,
			"", `worker "nas" did not finish in time`, "timeout"},
		{"busy", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "4 probes"), domain.ErrUnavailable,
			"", `worker "nas" is busy`, "busy"},
		{"already exists", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS, "config file already exists"), domain.ErrFailedPrecondition,
			"", "config file already exists", "already_exists"},
		{"a tool failure that echoes secrets", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
			"Fatal: s3 key "+remoteSecret+" rejected, password "+repoPassword), domain.ErrFailedPrecondition,
			"", "rejected", "execution_failed"},
		{"a long message is clipped", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, long), domain.ErrFailedPrecondition,
			"", strings.Repeat("é", 500) + "…", "execution_failed"},
		{"a failure without details", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED, ""), domain.ErrFailedPrecondition,
			"", "the worker reported a failure without details", "unspecified"},
		{"cancelled on the worker", func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
			return &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED}, nil
		}, domain.ErrUnavailable, "", "was stopped", "cancelled"},
		{"the transport: unavailable", func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
			return nil, errors.Join(domain.ErrUnavailable, errors.New(`worker "nas" did not answer in time`))
		}, domain.ErrUnavailable, "", "did not answer in time", ""},
		{"the transport: an error that echoes a secret", func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
			return nil, errors.Join(domain.ErrFailedPrecondition, errors.New("bad spec "+repoPassword))
		}, domain.ErrFailedPrecondition, "", "bad spec", ""},
		{"the caller went away", func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
			return nil, context.Canceled
		}, context.Canceled, "", "", ""},
	}
	for _, tt := range tests {
		e.transport.answer = tt.answer
		_, err := e.runner.Verify(ctx, alice.UserID, target, false)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: err = %v, want %v with %q", tt.name, err, tt.want, tt.msg)
			continue
		}
		var verr *domain.ValidationError
		if tt.field != "" && (!errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != tt.field) {
			t.Errorf("%s: err = %v, want a validation error on %q", tt.name, err, tt.field)
		}
		var pe *restic.ProbeError
		if got := errors.As(err, &pe); got != (tt.code != "") || (got && pe.Code != tt.code) {
			t.Errorf("%s: probe error = %+v, want code %q", tt.name, pe, tt.code)
		}
		if pe != nil {
			e.noSecrets(pe.Message)
		}
		// Exactly one domain class, so the API maps it unambiguously.
		classes := 0
		for _, c := range []error{domain.ErrInvalidArgument, domain.ErrFailedPrecondition, domain.ErrUnavailable, domain.ErrNotFound} {
			if errors.Is(err, c) {
				classes++
			}
		}
		if classes > 1 {
			t.Errorf("%s: err = %v matches %d domain classes", tt.name, err, classes)
		}
		e.noSecrets(err.Error())
	}
}

func TestSnapshotsAndList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	remote := e.remotes.s3("")
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	ctx := context.Background()
	target := restic.Target{RepositoryID: domain.NewResticRepoID(), RemoteID: remote, Path: "repo", Password: repoPassword}
	at := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)

	e.transport.answer = func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		return &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Output: &syncrv1.ProbeResult_Snapshots{
			Snapshots: &syncrv1.ResticSnapshotsOutput{Truncated: true, Snapshots: []*syncrv1.ResticSnapshot{
				{
					Id: strings.Repeat("1f", 32), Time: timestamppb.New(at), Host: "nas-01", Username: "root", Parent: strings.Repeat("0e", 32),
					ProgramVersion: "restic 0.19.1", Paths: []string{`C:\Users\ada`}, Tags: []string{"nightly"},
					Summary: &syncrv1.ResticSnapshotSummary{
						BackupStart: timestamppb.New(at), BackupEnd: timestamppb.New(at.Add(time.Minute)),
						FilesNew: 1, FilesChanged: 2, FilesUnmodified: 3, DirsNew: 4, DirsChanged: 5, DirsUnmodified: 6,
						DataBlobs: 7, TreeBlobs: 8, DataAddedBytes: 9, DataAddedPackedBytes: 10, TotalFilesProcessed: 11, TotalBytesProcessed: 12,
					},
				},
				{Id: strings.Repeat("2f", 32)},
			}},
		}}, nil
	}
	snaps, err := e.runner.Snapshots(ctx, alice.UserID, target)
	if err != nil {
		t.Fatal(err)
	}
	if !snaps.Truncated || snaps.WorkerID != nas || snaps.WorkerName != "nas" || len(snaps.Snapshots) != 2 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	s := snaps.Snapshots[0]
	want := restic.SnapshotSummary{
		BackupStart: at, BackupEnd: at.Add(time.Minute), FilesNew: 1, FilesChanged: 2, FilesUnmodified: 3,
		DirsNew: 4, DirsChanged: 5, DirsUnmodified: 6, DataBlobs: 7, TreeBlobs: 8, DataAdded: 9, DataAddedPacked: 10,
		TotalFilesProcessed: 11, TotalBytesProcessed: 12,
	}
	if s.ID != strings.Repeat("1f", 32) || !s.Time.Equal(at) || s.Hostname != "nas-01" || s.Username != "root" || s.Parent != strings.Repeat("0e", 32) ||
		s.ProgramVersion != "restic 0.19.1" || s.Paths[0] != `C:\Users\ada` || s.Tags[0] != "nightly" || s.Summary == nil || *s.Summary != want {
		t.Errorf("snapshot = %+v, summary %+v", s, s.Summary)
	}
	if snaps.Snapshots[1].Summary != nil || !snaps.Snapshots[1].Time.IsZero() {
		t.Errorf("a snapshot without summary = %+v", snaps.Snapshots[1])
	}
	if op := e.transport.last(t).probe.GetSpec().GetResticSnapshots(); op.GetMaxSnapshots() != 200 || op.GetRepository().GetLocation().GetPath() != "repo" {
		t.Errorf("snapshots op = %v", op.GetMaxSnapshots())
	}

	e.transport.answer = func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		return &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Output: &syncrv1.ProbeResult_Listing{
			Listing: &syncrv1.ProbeListing{Path: "/srv/photos", Truncated: true, Entries: []*syncrv1.ProbeEntry{
				{Name: "2025", Path: "/srv/photos/2025", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, Mode: "drwxr-xr-x"},
				{Name: "a.jpg", Path: "/srv/photos/a.jpg", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE, Size: 42, ModTime: timestamppb.New(at)},
				{Name: "link", Path: "/srv/photos/link", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK},
				{Name: "fifo", Path: "/srv/photos/fifo", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER},
				{Name: "odd", Path: "/srv/photos/odd"},
			}},
		}}, nil
	}
	l, err := e.runner.List(ctx, alice.UserID, target, "latest", "/srv/photos")
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []restic.EntryType{restic.EntryDir, restic.EntryFile, restic.EntrySymlink, restic.EntryOther, restic.EntryOther}
	if l.Path != "/srv/photos" || !l.Truncated || l.WorkerID != nas || len(l.Entries) != len(wantTypes) {
		t.Fatalf("listing = %+v", l)
	}
	for i, want := range wantTypes {
		if l.Entries[i].Type != want {
			t.Errorf("entry %d type = %q, want %q", i, l.Entries[i].Type, want)
		}
	}
	if f := l.Entries[1]; f.Name != "a.jpg" || f.Path != "/srv/photos/a.jpg" || f.Size != 42 || !f.ModTime.Equal(at) || l.Entries[0].Mode != "drwxr-xr-x" {
		t.Errorf("file entry = %+v", f)
	}
	if op := e.transport.last(t).probe.GetSpec().GetResticLs(); op.GetSnapshotId() != "latest" || op.GetPath() != "/srv/photos" || op.GetMaxEntries() != 100 {
		t.Errorf("ls op = %v %v %v", op.GetSnapshotId(), op.GetPath(), op.GetMaxEntries())
	}
	e.noSecrets()
}

func TestNewRunnerValidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	deps := probe.Deps{Transport: e.transport, Workers: e.workers, Jobs: e.jobs, Remotes: e.remotes, Limiter: e.limiter, IDs: &idgen.Sequential{}}
	if _, err := probe.NewRunner(probe.Deps{}, probe.Config{}); err == nil {
		t.Error("missing dependencies accepted")
	}
	for name, cfg := range map[string]probe.Config{
		"timeout too short":      {Timeout: time.Millisecond},
		"timeout too long":       {ResticTimeout: 2 * time.Minute},
		"scan timeout too short": {ScanTimeout: 999 * time.Millisecond},
		"scan timeout too long":  {ScanTimeout: link.MaxProbeTimeout + time.Second},
		"too many entries":       {MaxEntries: probe.MaxListEntries + 1},
		"negative snapshots":     {MaxSnapshots: -1},
		"negative max entries":   {MaxEntries: -5},
	} {
		if _, err := probe.NewRunner(deps, cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := probe.NewRunner(deps, probe.Config{}); err != nil {
		t.Errorf("defaults: %v", err)
	}
	if d := probe.DefaultConfig(); d.Timeout != 30*time.Second || d.ResticTimeout != 60*time.Second || d.ScanTimeout != 45*time.Second ||
		d.MaxEntries != 2000 || d.MaxSnapshots != 5000 {
		t.Errorf("defaults = %+v", d)
	}
}
