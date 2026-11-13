package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
)

// gateTools is fakeTools whose restic Find runs until gate is closed
// (complete) or its context ends: at the context's deadline it returns
// one match with complete=false, as the real probers do; when cancelled,
// the context's error, unless partialAlways makes it return the partial
// result anyway. sleep, when set, ignores the context and sleeps that
// long first (an operation stuck past its soft deadline).
type gateTools struct {
	*fakeTools
	gate          chan struct{}
	partialAlways bool
	sleep         time.Duration

	mu        sync.Mutex
	deadlines []time.Duration
}

func (g *gateTools) Find(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error) {
	if d, ok := ctx.Deadline(); ok {
		g.mu.Lock()
		g.deadlines = append(g.deadlines, time.Until(d))
		g.mu.Unlock()
	}
	partial := &syncrv1.ResticFindOutput{Matches: []*syncrv1.ResticFindMatch{{SnapshotId: "s1", Entry: &syncrv1.ProbeEntry{Path: "/a"}}}}
	if g.sleep > 0 {
		time.Sleep(g.sleep)
		return partial, nil
	}
	select {
	case <-g.gate:
		return &syncrv1.ResticFindOutput{Complete: true, SnapshotsSearched: 3}, nil
	case <-ctx.Done():
		if g.partialAlways || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return partial, nil
		}
		return nil, ctx.Err()
	}
}

func (g *gateTools) lastDeadline() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deadlines[len(g.deadlines)-1]
}

func newGateExecutor(t *testing.T, g *gateTools, mutate ...func(*Config)) *Executor {
	t.Helper()
	cfg := Config{Restic: g, Rclone: fakeRclone{g.fakeTools}, HostKeys: g.fakeTools}
	for _, m := range mutate {
		m(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func withTimeout(p *syncrv1.RunProbe, d time.Duration) *syncrv1.RunProbe {
	p.Timeout = durationpb.New(d)
	return p
}

func TestSoftMargin(t *testing.T) {
	t.Parallel()
	for timeout, want := range map[time.Duration]time.Duration{
		90 * time.Second: 9 * time.Second,
		45 * time.Second: 4500 * time.Millisecond,
		20 * time.Second: 2 * time.Second,
		10 * time.Second: 2 * time.Second,
		4 * time.Second:  2 * time.Second,
		3 * time.Second:  1500 * time.Millisecond,
		time.Second:      500 * time.Millisecond,
	} {
		if got := SoftMargin(timeout); got != want {
			t.Errorf("SoftMargin(%v) = %v, want %v", timeout, got, want)
		}
	}
}

// TestSoftDeadline: a heavy operation's context ends SoftMargin before the
// probe's timeout, and what it found then is an OK result.
func TestSoftDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := &gateTools{fakeTools: &fakeTools{}, gate: make(chan struct{})}
		e := newGateExecutor(t, g)
		for _, timeout := range []time.Duration{45 * time.Second, 10 * time.Second, 3 * time.Second} {
			start := time.Now()
			res := e.Probe(t.Context(), withTimeout(probeOf("restic_find", 0), timeout))
			soft := timeout - SoftMargin(timeout)
			if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || res.GetFind().GetComplete() || len(res.GetFind().GetMatches()) != 1 {
				t.Errorf("timeout %v: result = %v, want OK with the partial output", timeout, res)
			}
			if got := time.Since(start); got != soft || g.lastDeadline() != soft {
				t.Errorf("timeout %v: answered after %v (deadline %v), want %v", timeout, got, g.lastDeadline(), soft)
			}
		}
		// Without a timeout in the request: the default timeout's.
		e.Probe(t.Context(), probeOf("restic_find", 0))
		if want := DefaultTimeout - SoftMargin(DefaultTimeout); g.lastDeadline() != want {
			t.Errorf("default deadline = %v, want %v", g.lastDeadline(), want)
		}
		// A light operation keeps the whole timeout.
		local := localFunc(func(ctx context.Context) (localfs.Listing, error) {
			d, _ := ctx.Deadline()
			if time.Until(d) != 10*time.Second {
				t.Errorf("listing deadline = %v, want the probe's timeout", time.Until(d))
			}
			return localfs.Listing{}, nil
		})
		newExecutor(t, nil, local).Probe(t.Context(), withTimeout(probeOf("list_local", 0), 10*time.Second))
		// Finished in time: complete.
		close(g.gate)
		res := e.Probe(t.Context(), withTimeout(probeOf("restic_find", 0), 10*time.Second))
		if !res.GetFind().GetComplete() || res.GetFind().GetSnapshotsSearched() != 3 {
			t.Errorf("complete result = %v", res)
		}
	})
}

// TestHeavyCancelAndTimeout: only the soft deadline makes a partial
// result; a cancelled probe is CANCELLED and one still running at its own
// timeout fails with TIMEOUT, whatever the operation returns.
func TestHeavyCancelAndTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for _, partialAlways := range []bool{false, true} {
			g := &gateTools{fakeTools: &fakeTools{}, gate: make(chan struct{}), partialAlways: partialAlways}
			e := newGateExecutor(t, g)
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(time.Second, cancel)
			res := e.Probe(ctx, withTimeout(probeOf("restic_find", 0), 45*time.Second))
			if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED || res.GetOutput() != nil {
				t.Errorf("partialAlways=%v: cancelled result = %v", partialAlways, res)
			}
		}
		g := &gateTools{fakeTools: &fakeTools{}, sleep: 46 * time.Second}
		e := newGateExecutor(t, g)
		start := time.Now()
		mustFail(t, e.Probe(t.Context(), withTimeout(probeOf("restic_find", 0), 45*time.Second)), syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
		if time.Since(start) != 46*time.Second {
			t.Errorf("answered after %v", time.Since(start))
		}
	})
}

// TestHeavySlot: one heavy probe at a time by default, on top of a normal
// slot; more are BUSY at once, light probes are not affected.
func TestHeavySlot(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := &gateTools{fakeTools: &fakeTools{}, gate: make(chan struct{})}
		e := newGateExecutor(t, g, func(c *Config) { c.MaxConcurrent = 3 })
		done := make(chan *syncrv1.ProbeResult, 1)
		go func() { done <- e.Probe(t.Context(), withTimeout(probeOf("restic_find", 0), 45*time.Second)) }()
		synctest.Wait()

		for _, op := range []string{"restic_find", "restic_diff", "restic_measure", "measure_remote"} {
			res := e.Probe(t.Context(), probeOf(op, 0))
			mustFail(t, res, syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY)
			if !link.IsHeavyBusy(res.GetError().GetMessage()) {
				t.Errorf("%s: message = %q, want the heavy slot's", op, res.GetError().GetMessage())
			}
		}
		if res := e.Probe(t.Context(), probeOf("about", 0)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("a light probe beside a heavy one = %v", res)
		}
		close(g.gate)
		if res := <-done; !res.GetFind().GetComplete() {
			t.Errorf("first heavy probe = %v", res)
		}
		// A refused heavy probe took no slot: both kinds are free again.
		for range 2 {
			if res := e.Probe(t.Context(), probeOf("restic_diff", 0)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
				t.Errorf("after the heavy slot freed: %v", res)
			}
		}
	})
}

func TestHeavySlotsAreConfigurable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := &gateTools{fakeTools: &fakeTools{}, gate: make(chan struct{})}
		e := newGateExecutor(t, g, func(c *Config) { c.MaxConcurrent, c.MaxHeavy = 3, 2 })
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() { e.Probe(t.Context(), withTimeout(probeOf("restic_find", 0), 45*time.Second)) })
		}
		synctest.Wait()
		mustFail(t, e.Probe(t.Context(), probeOf("restic_diff", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY)
		// The third normal slot is still free for a light probe.
		if res := e.Probe(t.Context(), probeOf("about", 0)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("light probe = %v", res)
		}
		close(g.gate)
		wg.Wait()

		// With every normal slot taken a heavy probe is BUSY and leaves
		// the heavy slots alone. Its message says the slots are full, so
		// the controller does not word it as "already measuring".
		tools := &fakeTools{delay: time.Hour}
		e = newExecutor(t, tools, nil, func(c *Config) { c.MaxConcurrent = 1 })
		go e.Probe(t.Context(), probeOf("about", 0))
		synctest.Wait()
		res := e.Probe(t.Context(), probeOf("restic_find", 0))
		mustFail(t, res, syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY)
		if msg := res.GetError().GetMessage(); link.IsHeavyBusy(msg) || !strings.Contains(msg, link.ProbeBusySlots) {
			t.Errorf("slots full: message = %q", msg)
		}
		if n := len(e.heavy); n != 0 {
			t.Errorf("%d heavy slots held after a refused probe", n)
		}
		synctest.Wait()
	})
}

// measureLocal is a Local whose Measure is a function of its context.
type measureLocal func(ctx context.Context) (localfs.Measurement, error)

func (f measureLocal) List(context.Context, string, int, bool) (localfs.Listing, error) {
	return localfs.Listing{}, nil
}

func (f measureLocal) Measure(ctx context.Context, _ string, _ int) (localfs.Measurement, error) {
	return f(ctx)
}

// TestLocalMeasureSoftDeadline: a local measurement runs in a goroutine
// like a listing, but its outcome is awaited past the soft deadline, so
// the partial walk is answered; one stuck in a system call is answered at
// the probe's timeout and keeps both slots until it returns.
func TestLocalMeasureSoftDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		partial := measureLocal(func(ctx context.Context) (localfs.Measurement, error) {
			<-ctx.Done()
			time.Sleep(100 * time.Millisecond) // wrapping up takes a moment
			return localfs.Measurement{Path: "/srv", Files: 7, Children: []localfs.ChildMeasurement{{Name: "a", Files: 7}}}, nil
		})
		e := newExecutor(t, nil, partial)
		start := time.Now()
		res := e.Probe(t.Context(), withTimeout(probeOf("measure_local", 0), 20*time.Second))
		m := res.GetMeasurement()
		if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK || m.GetComplete() || m.GetFiles() != 7 || len(m.GetChildren()) != 1 {
			t.Errorf("result = %v", res)
		}
		if got, want := time.Since(start), 18*time.Second+100*time.Millisecond; got != want {
			t.Errorf("answered after %v, want %v", got, want)
		}

		release := make(chan struct{})
		stuck := measureLocal(func(context.Context) (localfs.Measurement, error) {
			<-release
			return localfs.Measurement{}, nil
		})
		e = newExecutor(t, &fakeTools{}, stuck, func(c *Config) { c.MaxConcurrent = 2 })
		start = time.Now()
		mustFail(t, e.Probe(t.Context(), withTimeout(probeOf("measure_local", 0), 20*time.Second)), syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT)
		if time.Since(start) != 20*time.Second {
			t.Errorf("stuck walk answered after %v, want the timeout", time.Since(start))
		}
		mustFail(t, e.Probe(t.Context(), probeOf("measure_local", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY)
		if res := e.Probe(t.Context(), probeOf("about", 0)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("light probe beside a stuck walk = %v", res)
		}
		close(release)
		synctest.Wait()
		if res := e.Probe(t.Context(), withTimeout(probeOf("measure_local", 0), 20*time.Second)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Errorf("after the walk returned: %v", res)
		}
	})
}

func TestHeavyBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		op        string
		requested int32
		want      int
	}{
		{"restic_find", 0, DefaultMatches},
		{"restic_find", -3, DefaultMatches},
		{"restic_find", 10, 10},
		{"restic_find", MaxMatches + 1, MaxMatches},
		{"restic_diff", 0, DefaultChanges},
		{"restic_diff", 7, 7},
		{"restic_diff", 1 << 30, MaxChanges},
		{"restic_measure", 0, 100}, // children: MaxEntries
		{"measure_remote", 0, 100},
	}
	for _, tc := range tests {
		tools := &fakeTools{}
		e := newExecutor(t, tools, nil, func(c *Config) { c.MaxEntries = 100 })
		p := probeOf(tc.op, tc.requested)
		before := proto.Clone(p)
		e.Probe(t.Context(), p)
		if tools.bound != tc.want {
			t.Errorf("%s with %d: the tool was given %d, want %d", tc.op, tc.requested, tools.bound, tc.want)
		}
		if !proto.Equal(p, before) {
			t.Errorf("%s: the executor changed the caller's request", tc.op)
		}
	}
	local := &fakeLocal{}
	newExecutor(t, nil, local, func(c *Config) { c.MaxEntries = 100 }).Probe(t.Context(), probeOf("measure_local", 0))
	if local.limit != 100 {
		t.Errorf("local measurement children = %d, want 100", local.limit)
	}
}

// bigTools returns heavy results with n items of long names, regardless
// of the bounds it is given (the executor enforces them too).
type bigTools struct {
	*fakeTools
	n int
}

func (b bigTools) name(i int) string { return fmt.Sprintf("%s-%05d", strings.Repeat("n", 60), i) }

func (b bigTools) Measure(_ context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticMeasure, _ int) (*syncrv1.ProbeMeasurement, error) {
	m := &syncrv1.ProbeMeasurement{Path: "/", Complete: true}
	for i := range b.n {
		m.Children = append(m.Children, &syncrv1.ProbeChildMeasurement{Name: b.name(i), Bytes: int64(b.n - i)})
	}
	return m, nil
}

func (b bigTools) Find(_ context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error) {
	f := &syncrv1.ResticFindOutput{Complete: true, SnapshotsSearched: 2}
	for i := range b.n {
		f.Matches = append(f.Matches, &syncrv1.ResticFindMatch{SnapshotId: strings.Repeat("a", 64), Entry: &syncrv1.ProbeEntry{Path: "/" + b.name(i)}})
	}
	return f, nil
}

func (b bigTools) Diff(_ context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticDiff) (*syncrv1.ResticDiffOutput, error) {
	d := &syncrv1.ResticDiffOutput{Complete: true, Stats: &syncrv1.ResticDiffStats{AddedFiles: int64(b.n)}}
	for i := range b.n {
		d.Changes = append(d.Changes, &syncrv1.ResticDiffChange{Path: "/" + b.name(i), Modifier: syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED})
	}
	return d, nil
}

func TestHeavyResultsAreCapped(t *testing.T) {
	t.Parallel()
	// The bounds: more than asked for is cut and marked.
	e, err := New(Config{Restic: bigTools{&fakeTools{}, 300}, MaxEntries: 50})
	if err != nil {
		t.Fatal(err)
	}
	m := e.Probe(t.Context(), probeOf("restic_measure", 0)).GetMeasurement()
	if len(m.GetChildren()) != 50 || !m.GetChildrenTruncated() || m.GetChildren()[0].GetBytes() != 300 {
		t.Errorf("measurement: %d children, truncated %v", len(m.GetChildren()), m.GetChildrenTruncated())
	}
	f := e.Probe(t.Context(), probeOf("restic_find", 20)).GetFind()
	if len(f.GetMatches()) != 20 || !f.GetTruncated() || !f.GetComplete() {
		t.Errorf("find: %d matches, truncated %v", len(f.GetMatches()), f.GetTruncated())
	}
	d := e.Probe(t.Context(), probeOf("restic_diff", 30)).GetDiff()
	if len(d.GetChanges()) != 30 || !d.GetTruncated() || d.GetStats() == nil {
		t.Errorf("diff: %d changes, truncated %v", len(d.GetChanges()), d.GetTruncated())
	}

	// The result size: the last items go (the smallest folders, the
	// oldest snapshots' matches, the last changes).
	const limit = 16 << 10
	e, err = New(Config{Restic: bigTools{&fakeTools{}, 3000}, MaxResultBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	res := e.Probe(t.Context(), probeOf("restic_measure", 0))
	m = res.GetMeasurement()
	if proto.Size(res) > limit || !m.GetChildrenTruncated() || len(m.GetChildren()) == 0 || m.GetChildren()[0].GetBytes() != 3000 || !m.GetComplete() {
		t.Errorf("measurement: %d bytes, %d children, truncated %v", proto.Size(res), len(m.GetChildren()), m.GetChildrenTruncated())
	}
	res = e.Probe(t.Context(), probeOf("restic_find", 5000))
	f = res.GetFind()
	if proto.Size(res) > limit || !f.GetTruncated() || len(f.GetMatches()) == 0 || !strings.HasSuffix(f.GetMatches()[0].GetEntry().GetPath(), "-00000") {
		t.Errorf("find: %d bytes, %d matches, truncated %v", proto.Size(res), len(f.GetMatches()), f.GetTruncated())
	}
	res = e.Probe(t.Context(), probeOf("restic_diff", 5000))
	d = res.GetDiff()
	if proto.Size(res) > limit || !d.GetTruncated() || len(d.GetChanges()) == 0 || d.GetStats().GetAddedFiles() != 3000 || !d.GetComplete() {
		t.Errorf("diff: %d bytes, %d changes, truncated %v", proto.Size(res), len(d.GetChanges()), d.GetTruncated())
	}
}

func TestLocalMeasurementConversion(t *testing.T) {
	t.Parallel()
	local := measureLocal(func(context.Context) (localfs.Measurement, error) {
		return localfs.Measurement{
			Path: "/srv/bad\xffdir", Bytes: 10, Files: 3, Dirs: 2, ChildrenTruncated: true, Complete: true, Unreadable: 4,
			Children: []localfs.ChildMeasurement{{Name: "big", Bytes: 9, Files: 2, Dirs: 1}, {Name: "bad\xffname", Bytes: 1, Files: 1}},
		}, nil
	})
	m := newExecutor(t, nil, local).Probe(t.Context(), probeOf("measure_local", 0)).GetMeasurement()
	want := &syncrv1.ProbeMeasurement{
		Path: "/srv/bad�dir", Bytes: 10, Files: 3, Dirs: 2, ChildrenTruncated: true, Complete: true, Unreadable: 4,
		Children: []*syncrv1.ProbeChildMeasurement{{Name: "big", Bytes: 9, Files: 2, Dirs: 1}, {Name: "bad�name", Bytes: 1, Files: 1}},
	}
	if !proto.Equal(m, want) {
		t.Errorf("measurement = %v\nwant %v", m, want)
	}
	// A failed walk fails the probe with its code.
	failing := measureLocal(func(context.Context) (localfs.Measurement, error) {
		return localfs.Measurement{}, &localfs.PathError{Path: "/etc", Err: localfs.ErrNotAllowed}
	})
	mustFail(t, newExecutor(t, nil, failing).Probe(t.Context(), probeOf("measure_local", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED)
	// A panic in the walk is an internal error, and frees both slots.
	e := newExecutor(t, nil, panicLocal{}, func(c *Config) { c.MaxConcurrent = 1 })
	for range 2 {
		mustFail(t, e.Probe(t.Context(), probeOf("measure_local", 0)), syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL)
	}
}
