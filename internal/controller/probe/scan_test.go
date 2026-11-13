package probe_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// scans answers the heavy probes as a worker does: a measurement of the
// path it was asked for (complete unless partial), a search with one match
// and a comparison with one change of each kind; usage requests get a
// usage.
func scans(partial bool) func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		res := &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Elapsed: durationpb.New(1500 * time.Millisecond)}
		measured := func(path string) {
			res.Output = &syncrv1.ProbeResult_Measurement{Measurement: &syncrv1.ProbeMeasurement{
				Path: path, Bytes: 300, Files: 3, Dirs: 2, Complete: !partial, Unreadable: 1, ChildrenTruncated: partial,
				Children: []*syncrv1.ProbeChildMeasurement{{Name: "2026", Bytes: 200, Files: 2, Dirs: 1}, {Name: "2025", Bytes: 100, Files: 1}},
			}}
		}
		spec := p.GetSpec()
		switch {
		case spec.GetMeasureLocal() != nil:
			measured(spec.GetMeasureLocal().GetPath())
		case spec.GetMeasureRemote() != nil:
			measured(spec.GetMeasureRemote().GetLocation().GetPath())
		case spec.GetResticMeasure() != nil:
			measured(spec.GetResticMeasure().GetPath())
		case spec.GetResticFind() != nil:
			res.Output = &syncrv1.ProbeResult_Find{Find: &syncrv1.ResticFindOutput{
				Matches: []*syncrv1.ResticFindMatch{{SnapshotId: strings.Repeat("ab", 32), Entry: &syncrv1.ProbeEntry{
					Name: "a.jpg", Path: "/srv/a.jpg", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE, Size: 5,
					ModTime: timestamppb.New(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)),
				}}},
				Truncated: partial, Complete: !partial, SnapshotsSearched: 7,
			}}
		case spec.GetResticDiff() != nil:
			d := &syncrv1.ResticDiffOutput{Truncated: partial, Complete: !partial}
			for i, m := range []syncrv1.ResticDiffModifier{
				syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED,
				syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_METADATA,
				syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER,
				syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_UNSPECIFIED,
			} {
				d.Changes = append(d.Changes, &syncrv1.ResticDiffChange{Path: "/srv/" + string(rune('a'+i)), Modifier: m})
			}
			if !partial {
				d.Stats = &syncrv1.ResticDiffStats{ChangedFiles: 1, AddedFiles: 2, AddedDirs: 3, AddedBytes: 4, RemovedFiles: 5, RemovedDirs: 6, RemovedBytes: 7}
			}
			res.Output = &syncrv1.ProbeResult_Diff{Diff: d}
		case spec.GetAbout() != nil:
			res.Output = &syncrv1.ProbeResult_Usage{Usage: &syncrv1.ProbeUsage{Total: proto.Int64(1000), Used: proto.Int64(400)}}
		}
		return res, nil
	}
}

// okWithout answers every probe OK without an output.
func okWithout(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK}, nil
}

// beforeScans is the Hello of a worker built before Milestone 7.
func beforeScans() workers.Info {
	info := current()
	info.ProbeOps = slices.DeleteFunc(slices.Clone(info.ProbeOps), link.IsHeavyProbeOp)
	return info
}

func TestMeasureWorkerFolder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = scans(false)
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	old := e.workers.add("old", domain.WorkerOnline, beforeScans())
	away := e.workers.add("away", domain.WorkerOffline, current())
	ctx := context.Background()

	// Any user may measure any worker's folder, with the scan timeout.
	for _, actor := range []domain.Actor{alice, bob} {
		resp, err := e.svc.MeasureWorkerFolder(ctx, actor, nas, "/srv/photos")
		if err != nil {
			t.Fatal(err)
		}
		m := resp.GetMeasurement()
		if m.GetPath() != "/srv/photos" || m.GetBytes() != 300 || !m.GetComplete() || m.GetUnreadable() != 1 || len(m.GetChildren()) != 2 ||
			resp.GetWorker().GetId() != nas.String() || resp.GetWorker().GetName() != "nas" || resp.GetElapsed().AsDuration() != 1500*time.Millisecond {
			t.Errorf("response = %v", resp)
		}
		sent := e.transport.last(t)
		if sent.worker != nas || sent.probe.GetSpec().GetMeasureLocal().GetPath() != "/srv/photos" || sent.probe.GetTimeout().AsDuration() != 30*time.Second ||
			len(sent.probe.GetSpec().GetRemotes()) != 0 {
			t.Errorf("sent %v to %v", sent.probe, sent.worker)
		}
	}

	// A walk that stopped at the soft deadline is a success.
	e.transport.answer = scans(true)
	if resp, err := e.svc.MeasureWorkerFolder(ctx, alice, nas, "/srv"); err != nil || resp.GetMeasurement().GetComplete() || !resp.GetMeasurement().GetChildrenTruncated() {
		t.Errorf("partial = %v, %v", resp, err)
	}

	before := e.transport.count()
	var verr *domain.ValidationError
	if _, err := e.svc.MeasureWorkerFolder(ctx, alice, domain.WorkerID{}, ""); !errors.As(err, &verr) || len(verr.Fields) != 2 ||
		verr.Fields[0].Field != "worker_id" || verr.Fields[1].Field != "path" {
		t.Errorf("no worker, no path: %v", err)
	}
	if _, err := e.svc.MeasureWorkerFolder(ctx, alice, domain.NewWorkerID(), "/srv"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown worker: %v", err)
	}
	if _, err := e.svc.MeasureWorkerFolder(ctx, alice, old, "/srv"); !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), "upgrade it") {
		t.Errorf("a worker without the operation: %v", err)
	}
	if _, err := e.svc.MeasureWorkerFolder(ctx, alice, away, "/srv"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("offline worker: %v", err)
	}
	if e.transport.count() != before {
		t.Error("a refused measurement was sent")
	}

	// The worker's failures.
	for _, tt := range []struct {
		name   string
		answer func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error)
		want   error
		field  string
		msg    string
	}{
		{"busy", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "the worker is already measuring, searching or comparing"),
			domain.ErrUnavailable, "", `worker "nas" is already measuring or searching; try again in a moment`},
		{"timeout", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT, ""), domain.ErrUnavailable, "", "did not finish in time"},
		{"outside the roots", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "/etc"), domain.ErrInvalidArgument, "path", "outside"},
		{"missing", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "no such folder"), domain.ErrInvalidArgument, "path", "does not exist"},
		{"a file", failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "/srv/a.txt is not a folder"), domain.ErrFailedPrecondition, "", "is not a folder"},
		{"no output", okWithout, domain.ErrUnavailable, "", "answered without a measurement"},
	} {
		e.transport.answer = tt.answer
		_, err := e.svc.MeasureWorkerFolder(ctx, alice, nas, "/srv")
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: %v, want %v with %q", tt.name, err, tt.want, tt.msg)
		}
		if tt.field != "" && (!errors.As(err, &verr) || verr.Fields[0].Field != tt.field) {
			t.Errorf("%s: %v, want an error on %s", tt.name, err, tt.field)
		}
	}
	e.limiter.close()
	if _, err := e.svc.MeasureWorkerFolder(ctx, alice, nas, "/srv"); !errors.Is(err, domain.ErrRateLimited) {
		t.Errorf("rate limited: %v", err)
	}
}

func TestMeasureRemoteFolder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = scans(false)
	remote, disk := e.remotes.s3("bucket/base"), e.remotes.local("/srv/data")
	e.workers.add("old", domain.WorkerOnline, beforeScans())
	busy := e.workers.add("busy", domain.WorkerOnline, current())
	idle := e.workers.add("idle", domain.WorkerOnline, current())
	e.jobs.unfinished[busy] = 2
	ctx := context.Background()

	// A bucket: measure_remote on the least busy worker that can run it;
	// the path comes back relative to the remote's root.
	resp, err := e.svc.MeasureRemoteFolder(ctx, alice, remote, "/photos//", domain.WorkerID{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetMeasurement().GetPath() != "photos" || resp.GetMeasurement().GetFiles() != 3 || resp.GetWorker().GetId() != idle.String() ||
		resp.GetElapsed().AsDuration() != 1500*time.Millisecond {
		t.Errorf("response = %v", resp)
	}
	sent := e.transport.last(t)
	loc := sent.probe.GetSpec().GetMeasureRemote().GetLocation()
	if loc.GetPath() != "bucket/base/photos" || loc.GetRemote() == "" || sent.probe.GetTimeout().AsDuration() != 30*time.Second ||
		sent.probe.GetSpec().GetRemotes()[0].GetSecrets()["secret_access_key"] != remoteSecret {
		t.Errorf("sent %v", sent.probe.GetSpec().GetMeasureRemote())
	}
	if resp, err = e.svc.MeasureRemoteFolder(ctx, alice, remote, "", busy); err != nil || resp.GetMeasurement().GetPath() != "" || e.transport.last(t).worker != busy {
		t.Errorf("the root through a chosen worker = %v, %v", resp, err)
	}

	// A remote on a worker's disk: measure_local on the joined path, with
	// the worker the caller chose.
	var verr *domain.ValidationError
	if _, err := e.svc.MeasureRemoteFolder(ctx, alice, disk, "films", domain.WorkerID{}); !errors.As(err, &verr) ||
		verr.Fields[0].Field != "worker_id" || verr.Fields[0].Message != "choose the worker whose disk holds this remote" {
		t.Errorf("local remote without a worker: %v", err)
	}
	if resp, err = e.svc.MeasureRemoteFolder(ctx, alice, disk, "films", idle); err != nil || resp.GetMeasurement().GetPath() != "films" ||
		e.transport.last(t).probe.GetSpec().GetMeasureLocal().GetPath() != "/srv/data/films" {
		t.Errorf("local remote = %v, %v (sent %v)", resp, err, e.transport.last(t).probe.GetSpec())
	}

	// Owner scoping, admins included, and bad input: nothing is sent.
	before := e.transport.count()
	if _, err := e.svc.MeasureRemoteFolder(ctx, bob, remote, "", domain.WorkerID{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another user's remote: %v", err)
	}
	if _, err := e.svc.MeasureRemoteFolder(ctx, alice, remote, "a/../../b", domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "path" {
		t.Errorf("a path with ..: %v", err)
	}
	if _, err := e.svc.MeasureRemoteFolder(ctx, alice, domain.RemoteID{}, "", domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "remote_id" {
		t.Errorf("no remote: %v", err)
	}
	if e.transport.count() != before {
		t.Error("a refused measurement was sent")
	}

	// Partial results pass; a busy worker is unavailable; secrets never
	// leave.
	e.transport.answer = scans(true)
	if resp, err = e.svc.MeasureRemoteFolder(ctx, alice, remote, "photos", domain.WorkerID{}); err != nil || resp.GetMeasurement().GetComplete() {
		t.Errorf("partial = %v, %v", resp, err)
	}
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "busy")
	if _, err := e.svc.MeasureRemoteFolder(ctx, alice, remote, "photos", domain.WorkerID{}); !errors.Is(err, domain.ErrUnavailable) ||
		!strings.Contains(err.Error(), "already measuring or searching") {
		t.Errorf("busy: %v", err)
	}
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "AccessDenied for "+remoteSecret)
	_, err = e.svc.MeasureRemoteFolder(ctx, alice, remote, "photos", domain.WorkerID{})
	if !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("failure: %v", err)
	}
	e.noSecrets(err.Error())

	// No worker can measure: the pick says so.
	e2 := newEnv(t)
	e2.workers.add("old", domain.WorkerOnline, beforeScans())
	r2 := e2.remotes.s3("bucket")
	if _, err := e2.svc.MeasureRemoteFolder(ctx, alice, r2, "", domain.WorkerID{}); !errors.Is(err, probe.ErrNoWorker) || !strings.Contains(err.Error(), "upgrade a worker") {
		t.Errorf("only old workers: %v", err)
	}
}

func TestRemoteUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = scans(false)
	remote, disk := e.remotes.s3("bucket/base"), e.remotes.local("/srv/data")
	nas := e.workers.add("nas", domain.WorkerOnline, current())
	away := e.workers.add("away", domain.WorkerOffline, current())
	ctx := context.Background()

	resp, err := e.svc.RemoteUsage(ctx, alice, remote, domain.WorkerID{})
	if err != nil || resp.GetUsage().GetTotal() != 1000 || resp.GetUsage().GetUsed() != 400 || resp.GetWorker().GetId() != nas.String() {
		t.Fatalf("usage = %v, %v", resp, err)
	}
	sent := e.transport.last(t)
	if loc := sent.probe.GetSpec().GetAbout().GetLocation(); loc.GetPath() != "bucket/base" || loc.GetRemote() == "" || sent.probe.GetTimeout().AsDuration() != 20*time.Second {
		t.Errorf("sent %v with %v", loc, sent.probe.GetTimeout().AsDuration())
	}
	// A backend that cannot tell: an answer, not an error.
	e.transport.answer = func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		return &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK,
			Output: &syncrv1.ProbeResult_Usage{Usage: &syncrv1.ProbeUsage{Unsupported: true}}}, nil
	}
	if resp, err = e.svc.RemoteUsage(ctx, alice, remote, nas); err != nil || !resp.GetUsage().GetUnsupported() {
		t.Errorf("unsupported = %v, %v", resp, err)
	}

	// A remote on a worker's disk: the worker is required and checked, and
	// the usage is unsupported without a probe.
	before := e.transport.count()
	var verr *domain.ValidationError
	if _, err := e.svc.RemoteUsage(ctx, alice, disk, domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "worker_id" ||
		verr.Fields[0].Message != "choose the worker whose disk holds this remote" {
		t.Errorf("local remote without a worker: %v", err)
	}
	if resp, err = e.svc.RemoteUsage(ctx, alice, disk, nas); err != nil || !resp.GetUsage().GetUnsupported() || resp.GetWorker().GetName() != "nas" {
		t.Errorf("local remote = %v, %v", resp, err)
	}
	if _, err := e.svc.RemoteUsage(ctx, alice, disk, away); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("local remote on an offline worker: %v", err)
	}
	if _, err := e.svc.RemoteUsage(ctx, alice, disk, domain.NewWorkerID()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("local remote on an unknown worker: %v", err)
	}
	if _, err := e.svc.RemoteUsage(ctx, bob, remote, domain.WorkerID{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another user's remote: %v", err)
	}
	if _, err := e.svc.RemoteUsage(ctx, alice, domain.RemoteID{}, domain.WorkerID{}); !errors.As(err, &verr) || verr.Fields[0].Field != "remote_id" {
		t.Errorf("no remote: %v", err)
	}
	if e.transport.count() != before {
		t.Error("a probe was sent")
	}
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "connection refused")
	if _, err := e.svc.RemoteUsage(ctx, alice, remote, domain.WorkerID{}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("failure: %v", err)
	}
	e.transport.answer = okWithout
	if _, err := e.svc.RemoteUsage(ctx, alice, remote, domain.WorkerID{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("no usage in the answer: %v", err)
	}
	e.noSecrets()
}

func TestRunnerScans(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = scans(false)
	remote := e.remotes.s3("bucket")
	e.workers.add("old", domain.WorkerOnline, beforeScans())
	idle := e.workers.add("idle", domain.WorkerOnline, current())
	last := e.workers.add("last", domain.WorkerOnline, current())
	e.jobs.unfinished[last] = 3
	ctx := context.Background()
	target := restic.Target{RepositoryID: domain.NewResticRepoID(), RemoteID: remote, Path: "restic/photos", Password: repoPassword}
	e.jobs.lastByRepo[target.RepositoryID] = last

	// Measure: on the repository's last worker, with the scan timeout.
	m, err := e.runner.Measure(ctx, alice.UserID, target, "1a2b3c4d", "/srv")
	if err != nil {
		t.Fatal(err)
	}
	want := restic.Measurement{
		WorkerID: last, WorkerName: "last", Path: "/srv", Bytes: 300, Files: 3, Dirs: 2, Complete: true, Unreadable: 1, Elapsed: 1500 * time.Millisecond,
		Children: []restic.ChildMeasurement{{Name: "2026", Bytes: 200, Files: 2, Dirs: 1}, {Name: "2025", Bytes: 100, Files: 1}},
	}
	if !equalMeasurement(m, want) {
		t.Errorf("measurement = %+v, want %+v", m, want)
	}
	sent := e.transport.last(t)
	op := sent.probe.GetSpec().GetResticMeasure()
	if sent.worker != last || op.GetSnapshotId() != "1a2b3c4d" || op.GetPath() != "/srv" || op.GetRepository().GetPassword() != repoPassword ||
		op.GetRepository().GetLocation().GetPath() != "bucket/restic/photos" || sent.probe.GetTimeout().AsDuration() != 30*time.Second {
		t.Errorf("sent %v to %v", op.GetRepository().GetLocation(), sent.worker)
	}

	// Find: the query and the bound go to the worker; matches come back.
	q := restic.FindQuery{Patterns: []string{"*.jpg", "/srv/a b"}, IgnoreCase: true, SnapshotIDs: []string{"1a2b3c4d"}}
	f, err := e.runner.Find(ctx, alice.UserID, target, q)
	if err != nil {
		t.Fatal(err)
	}
	if f.WorkerID != last || !f.Complete || f.Truncated || f.SnapshotsSearched != 7 || len(f.Matches) != 1 || f.Matches[0].SnapshotID != strings.Repeat("ab", 32) ||
		f.Matches[0].Entry.Path != "/srv/a.jpg" || f.Matches[0].Entry.Type != restic.EntryFile || f.Matches[0].Entry.Size != 5 || f.Matches[0].Entry.ModTime.IsZero() {
		t.Errorf("find = %+v", f)
	}
	fop := e.transport.last(t).probe.GetSpec().GetResticFind()
	if !slices.Equal(fop.GetPatterns(), q.Patterns) || !fop.GetIgnoreCase() || !slices.Equal(fop.GetSnapshotIds(), q.SnapshotIDs) ||
		fop.GetMaxMatches() != probe.FindMatches || e.transport.last(t).probe.GetTimeout().AsDuration() != 30*time.Second {
		t.Errorf("find op = %v", fop)
	}

	// Diff through a chosen worker: every modifier converts.
	target.WorkerID = idle
	d, err := e.runner.Diff(ctx, alice.UserID, target, "1a2b3c4d", "5e6f7a8b", "/srv")
	if err != nil {
		t.Fatal(err)
	}
	mods := []restic.DiffModifier{restic.DiffAdded, restic.DiffRemoved, restic.DiffChanged, restic.DiffMetadata, restic.DiffType, restic.DiffOther, restic.DiffOther}
	if d.WorkerID != idle || !d.Complete || d.Truncated || len(d.Changes) != len(mods) || d.Stats == nil ||
		*d.Stats != (restic.DiffStats{ChangedFiles: 1, AddedFiles: 2, AddedDirs: 3, AddedBytes: 4, RemovedFiles: 5, RemovedDirs: 6, RemovedBytes: 7}) {
		t.Fatalf("diff = %+v", d)
	}
	for i, c := range d.Changes {
		if c.Modifier != mods[i] || c.Path != "/srv/"+string(rune('a'+i)) {
			t.Errorf("change %d = %+v, want %s", i, c, mods[i])
		}
	}
	dop := e.transport.last(t).probe.GetSpec().GetResticDiff()
	if dop.GetFromSnapshotId() != "1a2b3c4d" || dop.GetToSnapshotId() != "5e6f7a8b" || dop.GetPath() != "/srv" || dop.GetMaxChanges() != probe.DiffChanges ||
		e.transport.last(t).worker != idle {
		t.Errorf("diff op = %v", dop)
	}

	// Stopped early: partial results are successes.
	e.transport.answer = scans(true)
	if m, err := e.runner.Measure(ctx, alice.UserID, target, "1a2b3c4d", "/"); err != nil || m.Complete || !m.ChildrenTruncated {
		t.Errorf("partial measurement = %+v, %v", m, err)
	}
	if f, err := e.runner.Find(ctx, alice.UserID, target, q); err != nil || f.Complete || !f.Truncated {
		t.Errorf("partial find = %+v, %v", f, err)
	}
	if d, err := e.runner.Diff(ctx, alice.UserID, target, "1a2b3c4d", "5e6f7a8b", "/"); err != nil || d.Complete || !d.Truncated || d.Stats != nil {
		t.Errorf("partial diff = %+v, %v", d, err)
	}

	// Failures: busy, a missing snapshot (with the worker's code), no
	// output, and an old worker chosen explicitly.
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "already scanning")
	if _, err := e.runner.Find(ctx, alice.UserID, target, q); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "already measuring or searching") {
		t.Errorf("busy: %v", err)
	}
	e.transport.answer = failed(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, `restic diff: failed (exit code 1): no matching ID found for prefix "5e6f7a8b"`)
	var pe *restic.ProbeError
	if _, err := e.runner.Diff(ctx, alice.UserID, target, "1a2b3c4d", "5e6f7a8b", "/"); !errors.As(err, &pe) || pe.Code != "not_found" || !strings.Contains(pe.Message, "5e6f7a8b") {
		t.Errorf("missing snapshot: %v", err)
	}
	for name, call := range map[string]func() error{
		"measure": func() error { _, err := e.runner.Measure(ctx, alice.UserID, target, "1a2b3c4d", "/"); return err },
		"find":    func() error { _, err := e.runner.Find(ctx, alice.UserID, target, q); return err },
		"diff": func() error {
			_, err := e.runner.Diff(ctx, alice.UserID, target, "1a2b3c4d", "5e6f7a8b", "/")
			return err
		},
	} {
		e.transport.answer = okWithout
		if err := call(); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "answered without") {
			t.Errorf("%s without output: %v", name, err)
		}
	}
	target.WorkerID = e.workers.add("older", domain.WorkerOnline, beforeScans())
	before := e.transport.count()
	if _, err := e.runner.Measure(ctx, alice.UserID, target, "1a2b3c4d", "/"); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("an old worker: %v", err)
	}
	if _, err := e.runner.Find(ctx, bob.UserID, target, q); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("another user's remote: %v", err)
	}
	if e.transport.count() != before {
		t.Error("a refused scan was sent")
	}
	e.noSecrets()
}

func equalMeasurement(a, b restic.Measurement) bool {
	return a.WorkerID == b.WorkerID && a.WorkerName == b.WorkerName && a.Path == b.Path && a.Bytes == b.Bytes && a.Files == b.Files &&
		a.Dirs == b.Dirs && slices.Equal(a.Children, b.Children) && a.ChildrenTruncated == b.ChildrenTruncated &&
		a.Complete == b.Complete && a.Unreadable == b.Unreadable && a.Elapsed == b.Elapsed
}
