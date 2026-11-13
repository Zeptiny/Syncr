package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// scanCall is one Milestone 7 call: its request carries the IDs of ids
// (by field name), which the test may replace.
type scanCall struct {
	name string
	// fields are the ID fields the handler checks, required ones first.
	fields []string
	call   func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error
}

func scanCalls() []scanCall {
	return []scanCall{
		{"MeasureWorkerFolder", []string{"worker_id"}, func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error {
			_, err := e.probesFor(a).MeasureWorkerFolder(ctx, connect.NewRequest(&syncrv1.MeasureWorkerFolderRequest{WorkerId: ids["worker_id"], Path: "/srv"}))
			return err
		}},
		{"MeasureRemoteFolder", []string{"remote_id", "worker_id"}, func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error {
			_, err := e.probesFor(a).MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{
				RemoteId: ids["remote_id"], Path: "photos", WorkerId: ids["worker_id"],
			}))
			return err
		}},
		{"GetRemoteUsage", []string{"remote_id", "worker_id"}, func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error {
			_, err := e.probesFor(a).GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: ids["remote_id"], WorkerId: ids["worker_id"]}))
			return err
		}},
		{"MeasureSnapshotFolder", []string{"repository_id", "worker_id"}, func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error {
			_, err := e.resticFor(a).MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{
				RepositoryId: ids["repository_id"], SnapshotId: "1a2b3c4d", Path: "/srv", WorkerId: ids["worker_id"],
			}))
			return err
		}},
		{"FindInRepository", []string{"repository_id", "worker_id"}, func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error {
			_, err := e.resticFor(a).FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{
				RepositoryId: ids["repository_id"], Patterns: []string{"*.jpg"}, IgnoreCase: true, WorkerId: ids["worker_id"],
			}))
			return err
		}},
		{"CompareSnapshots", []string{"repository_id", "worker_id"}, func(ctx context.Context, e *testEnv, a domain.Actor, ids map[string]string) error {
			_, err := e.resticFor(a).CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{
				RepositoryId: ids["repository_id"], FromSnapshotId: "1a2b3c4d", ToSnapshotId: "5e6f7a8b", WorkerId: ids["worker_id"],
			}))
			return err
		}},
	}
}

// validIDs are well-formed IDs for every field of a scanCall.
func validIDs() map[string]string {
	return map[string]string{
		"worker_id": domain.NewWorkerID().String(), "remote_id": domain.NewRemoteID().String(),
		"repository_id": domain.NewResticRepoID().String(),
	}
}

// TestScanCallsAreNotAvailableYet: every signed-in user gets past the
// guard; a missing or malformed ID is InvalidArgument on its field (the
// first field is required), anything else Unimplemented until the use
// cases land.
func TestScanCallsAreNotAvailableYet(t *testing.T) {
	t.Parallel()
	f := &fakeProbes{}
	e := newEnv(t, func(d *Deps) { d.Probes, d.Restic = f, stubRestic{} })
	ctx := context.Background()
	for _, c := range scanCalls() {
		for _, a := range []domain.Actor{testUser, testAdmin} {
			err := c.call(ctx, e, a, validIDs())
			wantPastGuard(t, err)
			if codeOf(t, err) != connect.CodeUnimplemented {
				t.Errorf("%s as %v: %v, want Unimplemented", c.name, a.Role, err)
			}
		}
		for _, field := range c.fields {
			ids := validIDs()
			ids[field] = "nope"
			if err := c.call(ctx, e, testUser, ids); codeOf(t, err) != connect.CodeInvalidArgument || violation(err, field) == "" {
				t.Errorf("%s with a malformed %s: %v, want InvalidArgument on it", c.name, field, err)
			}
		}
		ids := validIDs()
		ids[c.fields[0]] = ""
		if err := c.call(ctx, e, testUser, ids); codeOf(t, err) != connect.CodeInvalidArgument || violation(err, c.fields[0]) == "" {
			t.Errorf("%s without %s: %v, want InvalidArgument on it", c.name, c.fields[0], err)
		}
		ids = validIDs()
		ids["worker_id"] = ""
		if c.fields[0] != "worker_id" {
			if err := c.call(ctx, e, testUser, ids); codeOf(t, err) != connect.CodeUnimplemented {
				t.Errorf("%s without a worker: %v, want the controller to choose one", c.name, err)
			}
		}
	}
	if f.count() != 0 {
		t.Error("a call reached the probe service")
	}
}

// TestHeavyCallsRefuseGet: measuring, searching and comparing start a scan
// on a worker, so they accept no GET (a cross-site link must not start
// one); reading a remote's usage does.
func TestHeavyCallsRefuseGet(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *Deps) { d.Probes, d.Restic = &fakeProbes{}, stubRestic{} })
	heavy := map[string]bool{
		syncrv1connect.ProbeServiceMeasureWorkerFolderProcedure:    true,
		syncrv1connect.ProbeServiceMeasureRemoteFolderProcedure:    true,
		syncrv1connect.ResticServiceMeasureSnapshotFolderProcedure: true,
		syncrv1connect.ResticServiceFindInRepositoryProcedure:      true,
		syncrv1connect.ResticServiceCompareSnapshotsProcedure:      true,
		syncrv1connect.ProbeServiceGetRemoteUsageProcedure:         false,
	}
	for _, name := range []string{syncrv1connect.ProbeServiceName, syncrv1connect.ResticServiceName} {
		sd := serviceDescriptor(t, name)
		for i := range sd.Methods().Len() {
			md := sd.Methods().Get(i)
			p := procedure(sd, md)
			want, ok := heavy[p]
			if !ok {
				continue
			}
			if got := idempotency(md) == descriptorpb.MethodOptions_NO_SIDE_EFFECTS; got == want {
				t.Errorf("%s: NO_SIDE_EFFECTS = %v, want %v", p, got, !want)
			}
			delete(heavy, p)
		}
	}
	if len(heavy) != 0 {
		t.Errorf("procedures not found: %v", heavy)
	}

	client := e.clientWith(testActorHeader, actorHeader(testUser, "session"))
	get := func(procedure string) int {
		t.Helper()
		q := url.Values{"connect": {"v1"}, "encoding": {"json"}, "message": {"{}"}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.srv.URL+procedure+"?"+q.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, p := range []string{
		syncrv1connect.ProbeServiceMeasureWorkerFolderProcedure, syncrv1connect.ProbeServiceMeasureRemoteFolderProcedure,
		syncrv1connect.ResticServiceMeasureSnapshotFolderProcedure, syncrv1connect.ResticServiceFindInRepositoryProcedure,
		syncrv1connect.ResticServiceCompareSnapshotsProcedure,
	} {
		if status := get(p); status != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d, want 405", p, status)
		}
	}
	// GetRemoteUsage takes GET: it reaches the handler, which refuses the
	// empty request.
	if status := get(syncrv1connect.ProbeServiceGetRemoteUsageProcedure); status != http.StatusBadRequest {
		t.Errorf("GET GetRemoteUsage: %d, want 400 (past the method check)", status)
	}
}

func idempotency(md protoreflect.MethodDescriptor) descriptorpb.MethodOptions_IdempotencyLevel {
	opts, ok := md.Options().(*descriptorpb.MethodOptions)
	if !ok {
		return descriptorpb.MethodOptions_IDEMPOTENCY_UNKNOWN
	}
	return opts.GetIdempotencyLevel()
}

func TestFindQueryFromProto(t *testing.T) {
	t.Parallel()
	req := &syncrv1.FindInRepositoryRequest{Patterns: []string{"*.jpg", "/srv/a b"}, IgnoreCase: true, SnapshotIds: []string{"1a2b3c4d"}}
	q := findQueryFromProto(req)
	if len(q.Patterns) != 2 || q.Patterns[1] != "/srv/a b" || !q.IgnoreCase || len(q.SnapshotIDs) != 1 || q.SnapshotIDs[0] != "1a2b3c4d" {
		t.Errorf("query = %+v", q)
	}
	req.Patterns[0] = "changed"
	if q.Patterns[0] != "*.jpg" {
		t.Error("the query aliases the request")
	}
	if q := findQueryFromProto(&syncrv1.FindInRepositoryRequest{}); len(q.Patterns) != 0 || q.IgnoreCase || len(q.SnapshotIDs) != 0 {
		t.Errorf("empty query = %+v", q)
	}
}

func TestMeasureSnapshotFolderResponse(t *testing.T) {
	t.Parallel()
	worker := domain.NewWorkerID()
	got := measureSnapshotFolderResponse(restic.Measurement{
		WorkerID: worker, WorkerName: "nas", Path: "/srv/photos", Bytes: 300, Files: 3, Dirs: 2,
		Children:          []restic.ChildMeasurement{{Name: "2025", Bytes: 100, Files: 1, Dirs: 0}, {Name: "2026", Bytes: 200, Files: 2, Dirs: 1}},
		ChildrenTruncated: true, Complete: false, Unreadable: 4, Elapsed: 1500 * time.Millisecond,
	})
	want := &syncrv1.MeasureSnapshotFolderResponse{
		Measurement: &syncrv1.ProbeMeasurement{
			Path: "/srv/photos", Bytes: 300, Files: 3, Dirs: 2,
			Children: []*syncrv1.ProbeChildMeasurement{
				{Name: "2025", Bytes: 100, Files: 1}, {Name: "2026", Bytes: 200, Files: 2, Dirs: 1},
			},
			ChildrenTruncated: true, Unreadable: 4,
		},
		Worker:  &syncrv1.ProbeWorkerRef{Id: worker.String(), Name: "nas"},
		Elapsed: durationpb.New(1500 * time.Millisecond),
	}
	if !proto.Equal(got, want) {
		t.Errorf("response = %v, want %v", got, want)
	}
	if m := measureSnapshotFolderResponse(restic.Measurement{Path: "/", Complete: true}); m.GetWorker() != nil || !m.GetMeasurement().GetComplete() ||
		len(m.GetMeasurement().GetChildren()) != 0 || m.GetMeasurement().GetUnreadable() != 0 || m.GetElapsed().AsDuration() != 0 {
		t.Errorf("empty measurement = %v", m)
	}
}

func TestFindResponse(t *testing.T) {
	t.Parallel()
	worker := domain.NewWorkerID()
	mod := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	got := findResponse(restic.FindResult{
		WorkerID: worker, WorkerName: "nas", Truncated: true, Complete: true, SnapshotsSearched: 12,
		Matches: []restic.Match{{SnapshotID: "ab", Entry: restic.Entry{Name: "a.jpg", Path: "/srv/a.jpg", Type: restic.EntryFile, Size: 5, ModTime: mod}}},
	})
	want := &syncrv1.FindInRepositoryResponse{
		Matches: []*syncrv1.ResticFindMatch{{SnapshotId: "ab", Entry: &syncrv1.ProbeEntry{
			Name: "a.jpg", Path: "/srv/a.jpg", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE, Size: 5, ModTime: ts(mod),
		}}},
		Truncated: true, Complete: true, SnapshotsSearched: 12,
		Worker: &syncrv1.ProbeWorkerRef{Id: worker.String(), Name: "nas"},
	}
	if !proto.Equal(got, want) {
		t.Errorf("response = %v, want %v", got, want)
	}
	if got := findResponse(restic.FindResult{SnapshotsSearched: 1 << 40}).GetSnapshotsSearched(); got != 1<<31-1 {
		t.Errorf("a huge count = %d, want it clamped", got)
	}
	if got := findResponse(restic.FindResult{SnapshotsSearched: -3}).GetSnapshotsSearched(); got != 0 {
		t.Errorf("a negative count = %d, want 0", got)
	}
}

func TestCompareResponse(t *testing.T) {
	t.Parallel()
	worker := domain.NewWorkerID()
	changes := []restic.Change{
		{Path: "/srv/new.txt", Modifier: restic.DiffAdded}, {Path: "/srv/old/", Modifier: restic.DiffRemoved},
		{Path: "/srv/a.txt", Modifier: restic.DiffChanged}, {Path: "/srv/b.txt", Modifier: restic.DiffMetadata},
		{Path: "/srv/c", Modifier: restic.DiffType}, {Path: "/srv/d", Modifier: restic.DiffOther},
		{Path: "/srv/e", Modifier: "something new"}, {Path: "/srv/f"},
	}
	got := compareResponse(restic.Comparison{
		WorkerID: worker, WorkerName: "nas", Changes: changes, Complete: true,
		Stats: &restic.DiffStats{ChangedFiles: 1, AddedFiles: 2, AddedDirs: 3, AddedBytes: 4, RemovedFiles: 5, RemovedDirs: 6, RemovedBytes: 7},
	})
	mods := []syncrv1.ResticDiffModifier{
		syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED,
		syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_METADATA,
		syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER,
		syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER,
	}
	want := &syncrv1.CompareSnapshotsResponse{
		Stats:    &syncrv1.ResticDiffStats{ChangedFiles: 1, AddedFiles: 2, AddedDirs: 3, AddedBytes: 4, RemovedFiles: 5, RemovedDirs: 6, RemovedBytes: 7},
		Complete: true, Worker: &syncrv1.ProbeWorkerRef{Id: worker.String(), Name: "nas"},
	}
	for i, c := range changes {
		want.Changes = append(want.Changes, &syncrv1.ResticDiffChange{Path: c.Path, Modifier: mods[i]})
	}
	if !proto.Equal(got, want) {
		t.Errorf("response = %v, want %v", got, want)
	}
	// Stopped early: no statistics.
	if got := compareResponse(restic.Comparison{Truncated: true}); got.GetStats() != nil || got.GetComplete() || !got.GetTruncated() {
		t.Errorf("partial comparison = %v", got)
	}
}
