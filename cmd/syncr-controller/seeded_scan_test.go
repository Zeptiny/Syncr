package main

import (
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/devseed"
)

// TestRunSeededScans: on a seeded controller Milestone 7's calls reach the
// canned probes through the probe and restic use cases: folder sizes of a
// worker, a remote (one on a worker's disk too) and a snapshot, a
// remote's usage, a search and a comparison. A worker built before them is
// refused, and another user reaches none of the admin's remotes and
// repositories.
func TestRunSeededScans(t *testing.T) {
	t.Parallel()
	s := startSeeded(t)
	ctx := t.Context()
	admin := cookieClient(http.DefaultTransport, s.login(devseed.AdminUsername, devseed.AdminPassword), true)
	alice := cookieClient(http.DefaultTransport, s.login(devseed.AliceUsername, devseed.AlicePassword), true)
	pc, rc := syncrv1connect.NewProbeServiceClient(admin, s.base), syncrv1connect.NewResticServiceClient(admin, s.base)

	workers, err := syncrv1connect.NewWorkerAdminServiceClient(admin, s.base).ListWorkers(ctx, connect.NewRequest(&syncrv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ws := map[string]string{}
	for _, w := range workers.Msg.GetWorkers() {
		ws[w.GetName()] = w.GetId()
	}
	remotes, err := syncrv1connect.NewRemoteServiceClient(admin, s.base).ListRemotes(ctx, connect.NewRequest(&syncrv1.ListRemotesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	rs := map[string]string{}
	for _, r := range remotes.Msg.GetRemotes() {
		rs[r.GetName()] = r.GetId()
	}
	repos, err := rc.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var photos string
	for _, r := range repos.Msg.GetRepositories() {
		if r.GetName() == "Photos vault" {
			photos = r.GetId()
		}
	}

	// A worker's folder, by any user; the big disk stops early.
	m, err := syncrv1connect.NewProbeServiceClient(alice, s.base).MeasureWorkerFolder(ctx, connect.NewRequest(&syncrv1.MeasureWorkerFolderRequest{WorkerId: ws["nas-01"], Path: "/srv/photos"}))
	if err != nil || m.Msg.GetMeasurement().GetFiles() != 107 || !m.Msg.GetMeasurement().GetComplete() || m.Msg.GetWorker().GetName() != "nas-01" {
		t.Fatalf("MeasureWorkerFolder = %v, %v", m, err)
	}
	if m, err := pc.MeasureWorkerFolder(ctx, connect.NewRequest(&syncrv1.MeasureWorkerFolderRequest{WorkerId: ws["nas-01"], Path: "/mnt"})); err != nil ||
		m.Msg.GetMeasurement().GetComplete() || m.Msg.GetElapsed().AsDuration() == 0 {
		t.Errorf("MeasureWorkerFolder(/mnt) = %v, %v", m, err)
	}
	_, err = pc.MeasureWorkerFolder(ctx, connect.NewRequest(&syncrv1.MeasureWorkerFolderRequest{WorkerId: ws["attic-nas"], Path: "/volume1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("MeasureWorkerFolder on attic-nas = %v, want FailedPrecondition", err)
	}

	// Remotes: a bucket, and a folder of a worker's disk (needs the worker).
	if m, err := pc.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: rs["MinIO archive"], Path: "photos"})); err != nil ||
		m.Msg.GetMeasurement().GetPath() != "photos" || m.Msg.GetMeasurement().GetFiles() != 107 {
		t.Errorf("MeasureRemoteFolder(MinIO) = %v, %v", m, err)
	}
	_, err = pc.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: rs["Photos"], Path: "2026"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("MeasureRemoteFolder(local, no worker) = %v", err)
	}
	if m, err := pc.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: rs["Photos"], Path: "2026", WorkerId: ws["nas-01"]})); err != nil ||
		m.Msg.GetMeasurement().GetPath() != "2026" || m.Msg.GetMeasurement().GetFiles() != 100 {
		t.Errorf("MeasureRemoteFolder(local) = %v, %v", m, err)
	}

	// Usage.
	if u, err := pc.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: rs["B2 offsite"]})); err != nil || u.Msg.GetUsage().GetUsed() == 0 {
		t.Errorf("GetRemoteUsage(B2) = %v, %v", u, err)
	}
	if u, err := pc.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: rs["MinIO archive"]})); err != nil || !u.Msg.GetUsage().GetUnsupported() {
		t.Errorf("GetRemoteUsage(MinIO) = %v, %v", u, err)
	}
	if u, err := pc.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: rs["Photos"], WorkerId: ws["nas-01"]})); err != nil || !u.Msg.GetUsage().GetUnsupported() {
		t.Errorf("GetRemoteUsage(local) = %v, %v", u, err)
	}

	// A snapshot: sizes, a search and a comparison with the previous one.
	snaps, err := rc.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: photos}))
	if err != nil || len(snaps.Msg.GetSnapshots()) < 2 {
		t.Fatalf("ListSnapshots = %v, %v", snaps, err)
	}
	newest, previous := snaps.Msg.GetSnapshots()[0].GetId(), snaps.Msg.GetSnapshots()[1].GetId()
	if m, err := rc.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: photos, SnapshotId: newest, Path: "/srv/photos"})); err != nil ||
		m.Msg.GetMeasurement().GetFiles() != 107 {
		t.Errorf("MeasureSnapshotFolder = %v, %v", m, err)
	}
	_, err = rc.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: photos, SnapshotId: "deadbeef", Path: "/"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "snapshot_id") {
		t.Errorf("MeasureSnapshotFolder(unknown snapshot) = %v", err)
	}
	f, err := rc.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: photos, Patterns: []string{"albums.json"}, IgnoreCase: true}))
	if err != nil || len(f.Msg.GetMatches()) != len(snaps.Msg.GetSnapshots()) || !f.Msg.GetComplete() {
		t.Errorf("FindInRepository = %v, %v", f, err)
	}
	d, err := rc.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: photos, FromSnapshotId: previous, ToSnapshotId: newest}))
	if err != nil || len(d.Msg.GetChanges()) < 4 || d.Msg.GetStats() == nil || !d.Msg.GetComplete() {
		t.Errorf("CompareSnapshots = %v, %v", d, err)
	}

	// Another user: none of it.
	apc, arc := syncrv1connect.NewProbeServiceClient(alice, s.base), syncrv1connect.NewResticServiceClient(alice, s.base)
	checks := map[string]error{}
	_, checks["MeasureRemoteFolder"] = apc.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: rs["MinIO archive"]}))
	_, checks["GetRemoteUsage"] = apc.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: rs["B2 offsite"]}))
	_, checks["MeasureSnapshotFolder"] = arc.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: photos, SnapshotId: newest}))
	_, checks["FindInRepository"] = arc.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: photos, Patterns: []string{"x"}}))
	_, checks["CompareSnapshots"] = arc.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: photos, FromSnapshotId: previous, ToSnapshotId: newest}))
	for what, err := range checks {
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("%s as another user = %v, want NotFound", what, err)
		}
	}
}
