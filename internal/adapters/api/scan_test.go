package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

// TestScanCallsCheckTheirIDs: every signed-in user gets past the guard to
// the use cases; a missing or malformed ID is InvalidArgument on its field
// (the first field is required) and reaches nothing; without worker_id the
// controller chooses one.
func TestScanCallsCheckTheirIDs(t *testing.T) {
	t.Parallel()
	f := &fakeProbes{}
	e := newResticEnv(t, func(d *Deps) { d.Probes = f })
	repo := e.createRepo("Photos vault")
	ctx := context.Background()
	ids := func() map[string]string {
		m := validIDs()
		m["repository_id"] = repo.GetId()
		return m
	}
	for _, c := range scanCalls() {
		for _, a := range []domain.Actor{testUser, testAdmin} {
			err := c.call(ctx, e.testEnv, a, ids())
			wantPastGuard(t, err)
			// The admin's request reaches the use cases, which do not find
			// testUser's repository for them.
			if a == testUser && err != nil || a == testAdmin && err != nil && codeOf(t, err) != connect.CodeNotFound {
				t.Errorf("%s as %v: %v", c.name, a.Role, err)
			}
		}
		probes, scans := f.count(), e.prober.calls()
		for _, field := range c.fields {
			m := ids()
			m[field] = "nope"
			if err := c.call(ctx, e.testEnv, testUser, m); codeOf(t, err) != connect.CodeInvalidArgument || violation(err, field) == "" {
				t.Errorf("%s with a malformed %s: %v, want InvalidArgument on it", c.name, field, err)
			}
		}
		m := ids()
		m[c.fields[0]] = ""
		if err := c.call(ctx, e.testEnv, testUser, m); codeOf(t, err) != connect.CodeInvalidArgument || violation(err, c.fields[0]) != "is required" {
			t.Errorf("%s without %s: %v, want InvalidArgument on it", c.name, c.fields[0], err)
		}
		if f.count() != probes || e.prober.calls() != scans {
			t.Errorf("%s: a malformed request reached the use cases", c.name)
		}
		if c.fields[0] != "worker_id" {
			m = ids()
			m["worker_id"] = ""
			if err := c.call(ctx, e.testEnv, testUser, m); err != nil {
				t.Errorf("%s without a worker: %v, want the controller to choose one", c.name, err)
			}
		}
	}
}

// TestProbeScans: the probe service's scans pass the caller and their
// arguments on and return the use cases' answers, partial ones included;
// their errors map like the other probes'.
func TestProbeScans(t *testing.T) {
	t.Parallel()
	e, f := probeEnv(t)
	ctx := context.Background()
	cl := e.probesFor(testUser)
	worker, remote := domain.NewWorkerID(), domain.NewRemoteID()

	// Any signed-in user may measure a worker's folder.
	for _, a := range []domain.Actor{testUser, testAdmin} {
		resp, err := e.probesFor(a).MeasureWorkerFolder(ctx, connect.NewRequest(&syncrv1.MeasureWorkerFolderRequest{WorkerId: worker.String(), Path: "/srv/photos"}))
		if err != nil {
			t.Fatal(err)
		}
		m := resp.Msg.GetMeasurement()
		if m.GetPath() != "/srv/photos" || m.GetComplete() || m.GetUnreadable() != 2 || len(m.GetChildren()) != 1 || resp.Msg.GetWorker().GetName() != "nas" ||
			resp.Msg.GetElapsed().AsDuration() != 2*time.Second {
			t.Errorf("MeasureWorkerFolder = %v", resp.Msg)
		}
		if c := f.last(t); c.actor != a || c.worker != worker || c.path != "/srv/photos" {
			t.Errorf("call = %+v", c)
		}
	}
	resp, err := cl.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: remote.String(), Path: "photos", WorkerId: worker.String()}))
	if err != nil || resp.Msg.GetMeasurement().GetPath() != "photos" || resp.Msg.GetMeasurement().GetComplete() {
		t.Errorf("MeasureRemoteFolder = %v, %v", resp, err)
	}
	if c := f.last(t); c.remote != remote || c.worker != worker || c.path != "photos" {
		t.Errorf("call = %+v", c)
	}
	usage, err := cl.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: remote.String()}))
	if err != nil || !usage.Msg.GetUsage().GetUnsupported() || usage.Msg.GetWorker().GetName() != "nas" {
		t.Errorf("GetRemoteUsage = %v, %v", usage, err)
	}
	if c := f.last(t); c.remote != remote || !c.worker.IsZero() {
		t.Errorf("call = %+v", c)
	}

	// Errors keep their class; people's messages pass.
	measure := func() error {
		_, err := cl.MeasureRemoteFolder(ctx, connect.NewRequest(&syncrv1.MeasureRemoteFolderRequest{RemoteId: remote.String()}))
		return err
	}
	usageOf := func() error {
		_, err := cl.GetRemoteUsage(ctx, connect.NewRequest(&syncrv1.GetRemoteUsageRequest{RemoteId: remote.String()}))
		return err
	}
	for _, tt := range []struct {
		name  string
		err   error
		code  connect.Code
		field string
		msg   string
	}{
		{"another user's remote", fmt.Errorf("remote %s: %w", remote, domain.ErrNotFound), connect.CodeNotFound, "", "remote or worker not found"},
		{"busy", fmt.Errorf("measure the remote's folder: %w: worker \"nas\" is already measuring or searching; try again in a moment", domain.ErrUnavailable),
			connect.CodeUnavailable, "", "is already measuring or searching; try again in a moment"},
		{"a local remote without a worker", &domain.ValidationError{Fields: []domain.FieldError{{Field: "worker_id", Message: "choose the worker whose disk holds this remote"}}},
			connect.CodeInvalidArgument, "worker_id", "choose the worker whose disk holds this remote"},
		{"a missing folder", &domain.ValidationError{Fields: []domain.FieldError{{Field: "path", Message: "does not exist"}}},
			connect.CodeInvalidArgument, "path", "does not exist"},
		{"no worker", fmt.Errorf("%w: no worker can run this request; upgrade a worker", domain.ErrFailedPrecondition), connect.CodeFailedPrecondition, "", "upgrade a worker"},
		{"rate limited", domain.ErrRateLimited, connect.CodeResourceExhausted, "", "too many attempts"},
	} {
		f.fail(tt.err)
		for name, call := range map[string]func() error{"MeasureRemoteFolder": measure, "GetRemoteUsage": usageOf} {
			err := call()
			if codeOf(t, err) != tt.code || !strings.Contains(err.Error(), tt.msg) || tt.field != "" && violation(err, tt.field) != tt.msg {
				t.Errorf("%s %s: %v, want %v with %q", name, tt.name, err, tt.code, tt.msg)
			}
		}
	}
}

// TestResticScans: the restic scans go through the restic use cases, which
// keep repositories owner-only and check the input, and return what the
// worker found, partial results included.
func TestResticScans(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.resticFor(testUser)
	r := e.createRepo("Photos vault")
	full := strings.Repeat("ab", 32)
	at := e.clock.Now().Add(-time.Hour)
	e.prober.measurement = restic.Measurement{Path: "/srv", Bytes: 30, Files: 3, Dirs: 1, Complete: false, Elapsed: 40 * time.Second,
		Children: []restic.ChildMeasurement{{Name: "photos", Bytes: 30, Files: 3}}}
	e.prober.found = restic.FindResult{Truncated: true, Complete: false, SnapshotsSearched: 12,
		Matches: []restic.Match{{SnapshotID: full, Entry: restic.Entry{Name: "a.jpg", Path: "/srv/a.jpg", Type: restic.EntryFile, Size: 5, ModTime: at}}}}
	e.prober.comparison = restic.Comparison{Truncated: true, Complete: true,
		Changes: []restic.Change{{Path: "/srv/new/", Modifier: restic.DiffAdded}, {Path: "/srv/link", Modifier: restic.DiffType}},
		Stats:   &restic.DiffStats{AddedDirs: 1, AddedFiles: 2, AddedBytes: 300}}

	m, err := c.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d", Path: "/srv", WorkerId: e.worker.String()}))
	if err != nil {
		t.Fatal(err)
	}
	if mm := m.Msg.GetMeasurement(); mm.GetPath() != "/srv" || mm.GetBytes() != 30 || mm.GetComplete() || len(mm.GetChildren()) != 1 ||
		m.Msg.GetWorker().GetId() != e.worker.String() || m.Msg.GetElapsed().AsDuration() != 40*time.Second {
		t.Errorf("MeasureSnapshotFolder = %v", m.Msg)
	}
	if got := e.prober.lastScan(); got != "measure 1a2b3c4d /srv" || e.prober.lastTarget().WorkerID != e.worker || e.prober.lastTarget().Password != repoPassword {
		t.Errorf("measure call = %q", got)
	}
	noPassword(t, "MeasureSnapshotFolderResponse", m.Msg)

	f, err := c.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: r.GetId(), Patterns: []string{"*.jpg"}, IgnoreCase: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Msg.GetTruncated() || f.Msg.GetComplete() || f.Msg.GetSnapshotsSearched() != 12 || len(f.Msg.GetMatches()) != 1 ||
		f.Msg.GetMatches()[0].GetSnapshotId() != full || f.Msg.GetMatches()[0].GetEntry().GetPath() != "/srv/a.jpg" || f.Msg.GetWorker().GetName() != "worker" {
		t.Errorf("FindInRepository = %v", f.Msg)
	}
	if got := e.prober.lastScan(); got != `find ["*.jpg"] true []` {
		t.Errorf("find call = %s", got)
	}

	d, err := c.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: r.GetId(), FromSnapshotId: "1a2b3c4d", ToSnapshotId: full}))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Msg.GetTruncated() || !d.Msg.GetComplete() || d.Msg.GetStats().GetAddedBytes() != 300 || len(d.Msg.GetChanges()) != 2 ||
		d.Msg.GetChanges()[1].GetModifier() != syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE {
		t.Errorf("CompareSnapshots = %v", d.Msg)
	}
	if got := e.prober.lastScan(); got != "diff 1a2b3c4d "+full+" /" {
		t.Errorf("diff call = %s", got)
	}

	// Owner-only, admins included: nothing reaches a worker.
	before := e.prober.calls()
	for _, a := range []domain.Actor{testOther, testAdmin} {
		oc := e.resticFor(a)
		checks := map[string]error{}
		_, checks["MeasureSnapshotFolder"] = oc.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d"}))
		_, checks["FindInRepository"] = oc.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: r.GetId(), Patterns: []string{"x"}}))
		_, checks["CompareSnapshots"] = oc.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: r.GetId(), FromSnapshotId: "1a2b3c4d", ToSnapshotId: "5e6f7a8b"}))
		for what, err := range checks {
			if codeOf(t, err) != connect.CodeNotFound {
				t.Errorf("%s as %s = %v, want NotFound", what, a.Role, err)
			}
		}
	}

	// Validation errors name their field.
	for _, tt := range []struct {
		name  string
		call  func() error
		field string
	}{
		{"measure latest", func() error {
			_, err := c.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: r.GetId(), SnapshotId: "latest"}))
			return err
		}, "snapshot_id"},
		{"measure a relative path", func() error {
			_, err := c.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d", Path: "srv"}))
			return err
		}, "path"},
		{"find nothing", func() error {
			_, err := c.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: r.GetId()}))
			return err
		}, "patterns"},
		{"find with a line break", func() error {
			_, err := c.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: r.GetId(), Patterns: []string{"a\nb"}}))
			return err
		}, "patterns"},
		{"find in a bad snapshot", func() error {
			_, err := c.FindInRepository(ctx, connect.NewRequest(&syncrv1.FindInRepositoryRequest{RepositoryId: r.GetId(), Patterns: []string{"x"}, SnapshotIds: []string{"latest"}}))
			return err
		}, "snapshot_ids"},
		{"compare latest", func() error {
			_, err := c.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: r.GetId(), FromSnapshotId: "latest", ToSnapshotId: "5e6f7a8b"}))
			return err
		}, "from_snapshot_id"},
		{"compare with itself", func() error {
			_, err := c.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: r.GetId(), FromSnapshotId: "5e6f7a8b", ToSnapshotId: "5e6f7a8b"}))
			return err
		}, "to_snapshot_id"},
		{"compare a folder with ..", func() error {
			_, err := c.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: r.GetId(), FromSnapshotId: "1a2b3c4d", ToSnapshotId: "5e6f7a8b", Path: "/a/../b"}))
			return err
		}, "path"},
	} {
		if err := tt.call(); codeOf(t, err) != connect.CodeInvalidArgument || violation(err, tt.field) == "" {
			t.Errorf("%s: %v, want InvalidArgument on %s", tt.name, err, tt.field)
		}
	}
	if e.prober.calls() != before {
		t.Error("a refused request reached a worker")
	}

	// The worker's answers: busy, and a snapshot it does not have.
	e.prober.fail(fmt.Errorf("compare the snapshots: %w: worker \"worker\" is already measuring or searching; try again in a moment", domain.ErrUnavailable))
	_, err = c.CompareSnapshots(ctx, connect.NewRequest(&syncrv1.CompareSnapshotsRequest{RepositoryId: r.GetId(), FromSnapshotId: "1a2b3c4d", ToSnapshotId: "5e6f7a8b"}))
	if codeOf(t, err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "already measuring or searching") {
		t.Errorf("busy: %v", err)
	}
	e.prober.fail(errors.Join(&domain.ValidationError{Fields: []domain.FieldError{{Field: "path", Message: "does not exist"}}},
		&restic.ProbeError{Code: "not_found", Message: `restic ls: failed (exit code 1): no matching ID found for prefix "1a2b3c4d"`}))
	_, err = c.MeasureSnapshotFolder(ctx, connect.NewRequest(&syncrv1.MeasureSnapshotFolderRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d"}))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "snapshot_id") != "is not a snapshot of this repository" || violation(err, "path") != "" {
		t.Errorf("unknown snapshot: %v", err)
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
