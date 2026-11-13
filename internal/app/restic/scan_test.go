package restic_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// problems returns the fields of err, a validation error, as "field:
// message".
func problems(err error) []string {
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		return nil
	}
	out := make([]string, len(verr.Fields))
	for i, f := range verr.Fields {
		out[i] = f.Field + ": " + f.Message
	}
	return out
}

// notFound is the prober's error for a worker's NOT_FOUND with msg.
func notFound(msg string) error {
	return errors.Join(
		&domain.ValidationError{Fields: []domain.FieldError{{Field: "path", Message: "does not exist"}}},
		&restic.ProbeError{Code: "not_found", Message: msg},
	)
}

func TestMeasure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.prober.measurement = restic.Measurement{WorkerID: e.worker, WorkerName: "nas-01", Path: "/srv", Bytes: 30, Files: 3, Complete: false,
		Children: []restic.ChildMeasurement{{Name: "photos", Bytes: 30, Files: 3}}}

	m, err := e.svc.Measure(ctx, e.alice, r.ID, "1a2b3c4d", "/srv", e.worker)
	if err != nil || m.Path != "/srv" || m.Bytes != 30 || m.Complete || len(m.Children) != 1 {
		t.Fatalf("Measure = %+v, %v (a partial result is a success)", m, err)
	}
	call := e.prober.scans[0]
	if call.op != "measure" || call.snapshot != "1a2b3c4d" || call.path != "/srv" || call.target.Password != sentinel ||
		call.target.WorkerID != e.worker || call.target.RepositoryID != r.ID || call.target.RemoteID != e.s3 || call.target.Path != "restic/photos" {
		t.Errorf("Measure call = %+v", call)
	}
	// An empty path is the root, and the prober's empty path is filled.
	e.prober.measurement.Path = ""
	if m, err := e.svc.Measure(ctx, e.alice, r.ID, strings.Repeat("a", 64), "", domain.WorkerID{}); err != nil || m.Path != "/" || e.prober.scans[1].path != "/" {
		t.Errorf("Measure(root) = %+v, %v", m, err)
	}

	calls := len(e.prober.scans)
	for _, tt := range []struct{ snapshot, path, field string }{
		{"latest", "/", "snapshot_id"}, {"1a2b3c4", "/", "snapshot_id"}, {"1A2B3C4D", "/", "snapshot_id"},
		{"1a2b3c4d", "srv", "path"}, {"1a2b3c4d", "/srv/../etc", "path"}, {"1a2b3c4d", "/srv\x00", "path"},
	} {
		_, err := e.svc.Measure(ctx, e.alice, r.ID, tt.snapshot, tt.path, domain.WorkerID{})
		if got := problems(err); len(got) != 1 || !strings.HasPrefix(got[0], tt.field+":") {
			t.Errorf("Measure(%q, %q) = %v, want a problem on %s", tt.snapshot, tt.path, err, tt.field)
		}
	}
	// Owner scoping: another user's repository is not found, admins
	// included, whatever the input.
	for _, a := range []domain.Actor{e.bob, e.admin} {
		if _, err := e.svc.Measure(ctx, a, r.ID, "latest", "/", domain.WorkerID{}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Measure as %v = %v, want NotFound", a.Role, err)
		}
	}
	if len(e.prober.scans) != calls {
		t.Error("a refused request reached the worker")
	}

	// The worker's failures.
	for _, tt := range []struct {
		name  string
		err   error
		field string
		msg   string
	}{
		{"unknown snapshot", notFound(`restic ls: failed (exit code 1): no matching ID found for prefix "1a2b3c4d"`), "snapshot_id", "is not a snapshot of this repository"},
		{"missing folder", notFound(`the snapshot has no folder "/srv"`), "path", "is not a folder in this snapshot"},
		{"outside the worker's folders", &restic.ProbeError{Code: "path_not_allowed", Message: "x"}, "worker_id", "the repository is outside the worker's allowed folders"},
	} {
		e.prober.scanErr = tt.err
		_, err := e.svc.Measure(ctx, e.alice, r.ID, "1a2b3c4d", "/srv", domain.WorkerID{})
		if got := problems(err); !slices.Equal(got, []string{tt.field + ": " + tt.msg}) {
			t.Errorf("%s: %v, want %s: %s", tt.name, err, tt.field, tt.msg)
		}
	}
	e.prober.scanErr = fmt.Errorf("%w: worker \"nas-01\" is already measuring or searching; try again in a moment (%s)", domain.ErrUnavailable, sentinel)
	_, err = e.svc.Measure(ctx, e.alice, r.ID, "1a2b3c4d", "/srv", domain.WorkerID{})
	if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "already measuring") {
		t.Errorf("busy = %v", err)
	}
	noSecret(t, err)
	e.prober.scanErr = errors.Join(domain.ErrFailedPrecondition, &restic.ProbeError{Code: "not_found", Message: "no snapshot with " + sentinel})
	_, err = e.svc.Measure(ctx, e.alice, r.ID, "1a2b3c4d", "/srv", domain.WorkerID{})
	noSecret(t, err)
}

func TestFind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.prober.found = restic.FindResult{WorkerID: e.worker, WorkerName: "nas-01", Truncated: true, SnapshotsSearched: 4,
		Matches: []restic.Match{{SnapshotID: strings.Repeat("ab", 32), Entry: restic.Entry{Name: "a.jpg", Path: "/srv/a.jpg"}}}}

	q := restic.FindQuery{Patterns: []string{"*.jpg", "/srv/a b/[*].txt"}, IgnoreCase: true, SnapshotIDs: []string{"1a2b3c4d"}}
	f, err := e.svc.Find(ctx, e.alice, r.ID, q, e.worker)
	if err != nil || len(f.Matches) != 1 || !f.Truncated || f.Complete || f.SnapshotsSearched != 4 {
		t.Fatalf("Find = %+v, %v", f, err)
	}
	call := e.prober.scans[0]
	if call.op != "find" || !slices.Equal(call.query.Patterns, q.Patterns) || !call.query.IgnoreCase || !slices.Equal(call.query.SnapshotIDs, q.SnapshotIDs) ||
		call.target.WorkerID != e.worker || call.target.Password != sentinel {
		t.Errorf("Find call = %+v", call)
	}
	q.Patterns[0] = "changed"
	if e.prober.scans[0].query.Patterns[0] != "*.jpg" {
		t.Error("the prober's query aliases the caller's")
	}
	// Every snapshot: no IDs.
	if _, err := e.svc.Find(ctx, e.alice, r.ID, restic.FindQuery{Patterns: []string{strings.Repeat("é", 128)}}, domain.WorkerID{}); err != nil {
		t.Errorf("256 bytes of UTF-8: %v", err)
	}

	ten := make([]string, 10)
	for i := range ten {
		ten[i] = fmt.Sprintf("*.%d", i)
	}
	if _, err := e.svc.Find(ctx, e.alice, r.ID, restic.FindQuery{Patterns: ten}, domain.WorkerID{}); err != nil {
		t.Errorf("ten patterns: %v", err)
	}
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = fmt.Sprintf("%08x", i)
	}
	if _, err := e.svc.Find(ctx, e.alice, r.ID, restic.FindQuery{Patterns: []string{"x"}, SnapshotIDs: hundred}, domain.WorkerID{}); err != nil {
		t.Errorf("a hundred snapshots: %v", err)
	}

	calls := len(e.prober.scans)
	for _, tt := range []struct {
		name string
		q    restic.FindQuery
		want string
	}{
		{"no pattern", restic.FindQuery{}, "patterns: must have at least one pattern"},
		{"eleven patterns", restic.FindQuery{Patterns: append(slices.Clone(ten), "*")}, "patterns: must have at most 10 patterns"},
		{"an empty pattern", restic.FindQuery{Patterns: []string{""}}, "patterns: must not be empty"},
		{"an empty second pattern", restic.FindQuery{Patterns: []string{"a", ""}}, "patterns: pattern 2 must not be empty"},
		{"a long pattern", restic.FindQuery{Patterns: []string{strings.Repeat("a", 257)}}, "patterns: must be at most 256 bytes"},
		{"invalid UTF-8", restic.FindQuery{Patterns: []string{"a\xffb"}}, "patterns: must be valid UTF-8"},
		{"NUL", restic.FindQuery{Patterns: []string{"a\x00b"}}, "patterns: must not contain NUL characters or line breaks"},
		{"a line break", restic.FindQuery{Patterns: []string{"a", "b\nc"}}, "patterns: pattern 2 must not contain NUL characters or line breaks"},
		{"a carriage return", restic.FindQuery{Patterns: []string{"b\rc"}}, "patterns: must not contain NUL characters or line breaks"},
		{"101 snapshots", restic.FindQuery{Patterns: []string{"x"}, SnapshotIDs: append(slices.Clone(hundred), "1a2b3c4d")}, "snapshot_ids: must name at most 100 snapshots"},
		{"latest", restic.FindQuery{Patterns: []string{"x"}, SnapshotIDs: []string{"latest"}}, "snapshot_ids: must be snapshot IDs of 8 to 64 hex digits"},
		{"a short ID", restic.FindQuery{Patterns: []string{"x"}, SnapshotIDs: []string{"1a2b3c4d", "1a2b"}}, "snapshot_ids: must be snapshot IDs of 8 to 64 hex digits"},
	} {
		_, err := e.svc.Find(ctx, e.alice, r.ID, tt.q, domain.WorkerID{})
		if got := problems(err); !slices.Equal(got, []string{tt.want}) {
			t.Errorf("%s: %v, want %s", tt.name, err, tt.want)
		}
	}
	if _, err := e.svc.Find(ctx, e.bob, r.ID, restic.FindQuery{}, domain.WorkerID{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Find as another user = %v", err)
	}
	if len(e.prober.scans) != calls {
		t.Error("a refused request reached the worker")
	}

	// A requested snapshot the repository does not have.
	e.prober.scanErr = notFound("the repository has no snapshot 5e6f7a8b")
	_, err = e.svc.Find(ctx, e.alice, r.ID, restic.FindQuery{Patterns: []string{"x"}, SnapshotIDs: []string{"1a2b3c4d", "5e6f7a8b"}}, domain.WorkerID{})
	if got := problems(err); !slices.Equal(got, []string{"snapshot_ids: 5e6f7a8b is not a snapshot of this repository"}) {
		t.Errorf("unknown snapshot = %v", err)
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.prober.comparison = restic.Comparison{WorkerID: e.worker, WorkerName: "nas-01", Truncated: true, Complete: true,
		Changes: []restic.Change{{Path: "/srv/a.txt", Modifier: restic.DiffChanged}}, Stats: &restic.DiffStats{ChangedFiles: 1}}

	c, err := e.svc.Compare(ctx, e.alice, r.ID, "1a2b3c4d", strings.Repeat("5e", 32), "", e.worker)
	if err != nil || len(c.Changes) != 1 || c.Stats == nil || !c.Truncated {
		t.Fatalf("Compare = %+v, %v", c, err)
	}
	call := e.prober.scans[0]
	if call.op != "diff" || call.from != "1a2b3c4d" || call.to != strings.Repeat("5e", 32) || call.path != "/" || call.target.WorkerID != e.worker || call.target.Password != sentinel {
		t.Errorf("Diff call = %+v", call)
	}
	// Stopped early: no statistics, still a success.
	e.prober.comparison = restic.Comparison{}
	if c, err := e.svc.Compare(ctx, e.alice, r.ID, "1a2b3c4d", "5e6f7a8b", "/srv/photos", domain.WorkerID{}); err != nil || c.Stats != nil || e.prober.scans[1].path != "/srv/photos" {
		t.Errorf("partial Compare = %+v, %v", c, err)
	}

	calls := len(e.prober.scans)
	for _, tt := range []struct {
		from, to, path string
		want           []string
	}{
		{"latest", "5e6f7a8b", "/", []string{"from_snapshot_id: must be a snapshot ID of 8 to 64 hex digits"}},
		{"1a2b3c4d", "", "/", []string{"to_snapshot_id: must be a snapshot ID of 8 to 64 hex digits"}},
		{"1a2b3c4d", "1a2b3c4d", "/", []string{"to_snapshot_id: must be another snapshot than from_snapshot_id"}},
		{"1a2b3c4d", "1a2b3c4d" + strings.Repeat("0", 56), "/", []string{"to_snapshot_id: must be another snapshot than from_snapshot_id"}},
		{"1a2b3c4d5e", "1a2b3c4d", "/", []string{"to_snapshot_id: must be another snapshot than from_snapshot_id"}},
		{"1a2b3c4d", "5e6f7a8b", "srv", []string{"path: must be a snapshot path starting with \"/\""}},
		{"1a2b3c4d", "5e6f7a8b", "/a/../b", []string{"path: must not contain \"..\" segments"}},
		{"x", "y", "/", []string{"from_snapshot_id: must be a snapshot ID of 8 to 64 hex digits", "to_snapshot_id: must be a snapshot ID of 8 to 64 hex digits"}},
	} {
		_, err := e.svc.Compare(ctx, e.alice, r.ID, tt.from, tt.to, tt.path, domain.WorkerID{})
		if got := problems(err); !slices.Equal(got, tt.want) {
			t.Errorf("Compare(%q, %q, %q) = %v, want %v", tt.from, tt.to, tt.path, err, tt.want)
		}
	}
	for _, a := range []domain.Actor{e.bob, e.admin} {
		if _, err := e.svc.Compare(ctx, a, r.ID, "1a2b3c4d", "5e6f7a8b", "/", domain.WorkerID{}); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Compare as %v = %v, want NotFound", a.Role, err)
		}
	}
	if len(e.prober.scans) != calls {
		t.Error("a refused request reached the worker")
	}

	// The worker's NOT_FOUND names the field it concerns.
	for _, tt := range []struct {
		msg  string
		want []string
	}{
		{`restic diff: failed (exit code 1): no matching ID found for prefix "1a2b3c4d"`, []string{"from_snapshot_id: is not a snapshot of this repository"}},
		{`restic diff: failed (exit code 1): no matching ID found for prefix "5e6f7a8b"`, []string{"to_snapshot_id: is not a snapshot of this repository"}},
		{"no snapshot found", []string{
			"from_snapshot_id: this or the other snapshot is not in this repository", "to_snapshot_id: this or the other snapshot is not in this repository",
		}},
		{"restic diff: failed (exit code 1): path srv/nope: not found", []string{"path: is not a folder in both snapshots"}},
		{"restic diff: failed (exit code 1): path srv/a.txt: not a directory", []string{"path: is not a folder in both snapshots"}},
	} {
		e.prober.scanErr = notFound(tt.msg)
		_, err := e.svc.Compare(ctx, e.alice, r.ID, "1a2b3c4d", "5e6f7a8b", "/srv/nope", domain.WorkerID{})
		if got := problems(err); !slices.Equal(got, tt.want) {
			t.Errorf("%q: %v, want %v", tt.msg, got, tt.want)
		}
	}
}
