package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Restic runs the restic probe operations (*restic.Prober).
//
// Measure, Find and Diff are heavy (link.HeavyProbeOps): their ctx carries
// a soft deadline, earlier than the probe's own. When that deadline passes
// they return what they found so far with complete=false and no error; when
// ctx is cancelled they return its error. Measure returns at most
// maxChildren child folders (children_truncated beyond) and counts the
// entries it could not read (unreadable) instead of failing.
type Restic interface {
	Snapshots(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticSnapshots) (*syncrv1.ResticSnapshotsOutput, error)
	Ls(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticLs) (*syncrv1.ProbeListing, error)
	Init(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticInit) (*syncrv1.ResticRepositoryConfig, error)
	Config(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticConfig) (*syncrv1.ResticRepositoryConfig, error)
	Measure(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticMeasure, maxChildren int) (*syncrv1.ProbeMeasurement, error)
	Find(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error)
	Diff(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticDiff) (*syncrv1.ResticDiffOutput, error)
}

// Rclone runs the rclone probe operations (*rclone.Prober). Measure is
// heavy and ends at its ctx's deadline as Restic's heavy operations do.
type Rclone interface {
	List(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeListRemote) (*syncrv1.ProbeListing, error)
	About(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeAbout) (*syncrv1.ProbeUsage, error)
	Measure(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeMeasureRemote, maxChildren int) (*syncrv1.ProbeMeasurement, error)
}

// Local lists and measures folders inside the worker's local_roots
// (*localfs.Policy). List should return ctx's error soon after ctx ends; a
// call it cannot interrupt is handled by the Executor (see Probe). Measure
// is heavy and ends at its ctx's deadline as Restic's heavy operations do
// (localfs.Measurement.Complete false).
type Local interface {
	List(ctx context.Context, path string, limit int, dirsOnly bool) (localfs.Listing, error)
	Measure(ctx context.Context, path string, maxChildren int) (localfs.Measurement, error)
}

// Defaults of Config.
const (
	DefaultMaxConcurrent  = 4
	DefaultMaxHeavy       = 1
	DefaultTimeout        = 30 * time.Second
	DefaultMaxEntries     = 5000
	DefaultEntries        = 2000
	DefaultMaxSnapshots   = 5000
	DefaultMaxResultBytes = 4 << 20
	// MaxMessageBytes bounds a result's error message.
	MaxMessageBytes = 1000
)

// Bounds of the heavy operations' results (link.HeavyProbeOps). A request
// that names no bound (0) gets the default; none gets more than the cap. A
// measurement returns at most Config.MaxEntries child folders.
const (
	DefaultMatches = 1000
	MaxMatches     = 5000
	DefaultChanges = 2000
	MaxChanges     = 5000
)

// Soft deadline of the heavy operations: they stop this long before the
// probe's timeout, so that what they found reaches the controller in time
// (see Probe).
const (
	// MinSoftMargin is the least time left between the soft deadline and
	// the probe's own.
	MinSoftMargin = 2 * time.Second
	// SoftMarginDivisor makes the margin a tenth of longer timeouts.
	SoftMarginDivisor = 10
)

// Config configures an Executor. A nil Restic, Rclone or Local leaves its
// operations unsupported (the tool could not be resolved).
type Config struct {
	Restic Restic
	Rclone Rclone
	Local  Local
	// HostKeys answers sftp_host_keys, which needs no tool: nil means an
	// SSHScanner with its defaults.
	HostKeys HostKeys

	// MaxConcurrent is how many probes run at once; more are refused as
	// BUSY. Zero means DefaultMaxConcurrent.
	MaxConcurrent int
	// MaxHeavy is how many heavy probes (link.HeavyProbeOps: measuring,
	// searching, comparing) run at once, each also holding one of the
	// MaxConcurrent slots; more are refused as BUSY. Zero means
	// DefaultMaxHeavy.
	MaxHeavy int
	// DefaultTimeout applies when RunProbe.timeout is unset, MaxTimeout
	// caps it. Zero means DefaultTimeout and link.MaxProbeTimeout, the cap
	// the controller applies too; MaxTimeout may not exceed it.
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	// MaxEntries caps the entries of a listing whatever the request asks
	// for; a request that asks for none gets DefaultEntries. Zero means
	// DefaultMaxEntries.
	MaxEntries int
	// MaxSnapshots caps (and defaults) the snapshots of a listing. Zero
	// means DefaultMaxSnapshots.
	MaxSnapshots int
	// MaxResultBytes bounds the encoded result: listings are trimmed to
	// fit and marked truncated. Zero means DefaultMaxResultBytes.
	MaxResultBytes int

	// Logger receives diagnostics (never specs or secrets). Nil discards
	// them.
	Logger *slog.Logger
}

// Executor runs probes. It is safe for concurrent use.
type Executor struct {
	cfg  Config
	log  *slog.Logger
	slot chan struct{}
	// heavy holds the slots of the heavy operations (Config.MaxHeavy).
	heavy chan struct{}
}

// New returns an Executor for cfg.
func New(cfg Config) (*Executor, error) {
	for name, v := range map[string]int{
		"MaxConcurrent": cfg.MaxConcurrent, "MaxHeavy": cfg.MaxHeavy, "MaxEntries": cfg.MaxEntries,
		"MaxSnapshots": cfg.MaxSnapshots, "MaxResultBytes": cfg.MaxResultBytes,
	} {
		if v < 0 {
			return nil, fmt.Errorf("probe: Config.%s is negative", name)
		}
	}
	if cfg.DefaultTimeout < 0 || cfg.MaxTimeout < 0 {
		return nil, errors.New("probe: a timeout is negative")
	}
	if cfg.MaxTimeout > link.MaxProbeTimeout {
		return nil, fmt.Errorf("probe: Config.MaxTimeout is above %v", link.MaxProbeTimeout)
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = DefaultMaxConcurrent
	}
	if cfg.MaxHeavy == 0 {
		cfg.MaxHeavy = DefaultMaxHeavy
	}
	if cfg.DefaultTimeout == 0 {
		cfg.DefaultTimeout = DefaultTimeout
	}
	if cfg.MaxTimeout == 0 {
		cfg.MaxTimeout = link.MaxProbeTimeout
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.MaxSnapshots == 0 {
		cfg.MaxSnapshots = DefaultMaxSnapshots
	}
	if cfg.MaxResultBytes == 0 {
		cfg.MaxResultBytes = DefaultMaxResultBytes
	}
	if cfg.HostKeys == nil {
		cfg.HostKeys = &SSHScanner{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Executor{cfg: cfg, log: log, slot: make(chan struct{}, cfg.MaxConcurrent), heavy: make(chan struct{}, cfg.MaxHeavy)}, nil
}

// Ops returns the probe ops this worker can run, in the wire contract's
// order: the names of the ProbeSpec.op fields whose tool is available.
func (e *Executor) Ops() []string {
	var ops []string
	for _, op := range link.ProbeOps() {
		if e.supported(op) {
			ops = append(ops, op)
		}
	}
	return ops
}

func (e *Executor) supported(op string) bool {
	switch op {
	case "list_local", "measure_local":
		return e.cfg.Local != nil
	case "list_remote", "about", "measure_remote":
		return e.cfg.Rclone != nil
	case "restic_snapshots", "restic_ls", "restic_init", "restic_config",
		"restic_measure", "restic_find", "restic_diff":
		return e.cfg.Restic != nil
	case "sftp_host_keys":
		return true
	}
	return false
}

// MaxTimeout returns the cap on a probe's timeout.
func (e *Executor) MaxTimeout() time.Duration { return e.cfg.MaxTimeout }

// Probe runs one probe and returns its result. It never fails: a failure is
// a result with status FAILED, an error code and a message scrubbed by the
// tool's prober; a probe stopped through ctx has status CANCELLED; one that
// outlives its timeout fails with TIMEOUT; one refused because too many are
// running fails with BUSY.
//
// A probe holds one of the MaxConcurrent slots while it runs; a heavy one
// (link.HeavyProbeOps: measuring, searching, comparing) also holds one of
// the MaxHeavy slots, and is refused as BUSY when either kind is taken. The
// tools' probes end their child process with their context. A local
// listing or measurement runs in a goroutine of its own that holds the
// slots: a filesystem call blocked in the kernel (a hung network mount)
// cannot be interrupted, so Probe answers at the deadline or cancellation
// and the operation gives the slots back only when that call returns.
// Stuck local operations are thus bounded by the slots, and a worker whose
// slots are all stuck answers BUSY.
//
// A heavy operation gets a soft deadline: its context ends SoftMargin
// before the probe's timeout, and the operation then returns what it found
// so far (complete=false), which Probe answers as OK. Only that inner
// deadline yields a partial result: a cancelled probe is CANCELLED and one
// still running at its own timeout fails with TIMEOUT, whatever the
// operation returned.
func (e *Executor) Probe(ctx context.Context, p *syncrv1.RunProbe) *syncrv1.ProbeResult {
	start := time.Now()
	res := &syncrv1.ProbeResult{ProbeId: p.GetProbeId()}
	finish := func(err error) *syncrv1.ProbeResult {
		res.Elapsed = durationpb.New(time.Since(start))
		if err == nil {
			res.Status = syncrv1.ProbeStatus_PROBE_STATUS_OK
			return res
		}
		res.Output = nil
		res.Status = syncrv1.ProbeStatus_PROBE_STATUS_FAILED
		res.Error = &syncrv1.JobError{Code: codeOf(err), Message: clip(err.Error(), MaxMessageBytes)}
		return res
	}

	op := link.ProbeOpOf(p.GetSpec())
	switch {
	case op == "":
		return finish(runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "the probe has no operation"))
	case !e.supported(op):
		return finish(runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND, "this worker cannot run %s probes", op))
	}
	heavy := link.IsHeavyProbeOp(op)
	s, busy := e.acquire(heavy)
	if busy != nil {
		return finish(busy)
	}
	defer s.release()

	timeout := e.cfg.DefaultTimeout
	if d := p.GetTimeout().AsDuration(); p.GetTimeout() != nil && d > 0 {
		timeout = d
	}
	timeout = min(timeout, e.cfg.MaxTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	opCtx := runCtx
	if heavy {
		var cancelSoft context.CancelFunc
		opCtx, cancelSoft = context.WithTimeout(runCtx, timeout-SoftMargin(timeout))
		defer cancelSoft()
	}

	err := e.run(opCtx, runCtx, op, p, res, s)
	switch {
	case err == nil && (!heavy || runCtx.Err() == nil):
		e.fit(res)
		return finish(nil)
	case ctx.Err() != nil:
		// The caller went away (CancelProbe, or the passive call ended).
		res.Elapsed = durationpb.New(time.Since(start))
		res.Output = nil
		res.Status = syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED
		res.Error = &syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED, Message: "the probe was cancelled"}
		return res
	case runCtx.Err() != nil:
		return finish(runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT, "the probe did not finish within %s", timeout))
	}
	return finish(err)
}

// SoftMargin is how long before a probe's timeout a heavy operation's soft
// deadline falls: a tenth of the timeout, at least MinSoftMargin, and at
// most half of it (a timeout under twice MinSoftMargin is halved).
func SoftMargin(timeout time.Duration) time.Duration {
	return min(max(MinSoftMargin, timeout/SoftMarginDivisor), timeout/2)
}

// acquire takes a concurrency slot and, for a heavy operation, a heavy
// slot too. It never waits: when either is taken it returns a BUSY error.
func (e *Executor) acquire(heavy bool) (*slot, error) {
	select {
	case e.slot <- struct{}{}:
	default:
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "%s", link.ProbeBusySlots)
	}
	s := &slot{ch: e.slot}
	if !heavy {
		return s, nil
	}
	select {
	case e.heavy <- struct{}{}:
		s.heavy = e.heavy
		return s, nil
	default:
		s.release()
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "%s", link.ProbeBusyHeavy)
	}
}

// slot is what a probe holds: a concurrency slot and, for a heavy
// operation, a heavy slot. Probe releases them when it returns, unless an
// operation took them over to release them itself.
type slot struct {
	ch    chan struct{}
	heavy chan struct{}
	taken bool
}

// take hands the slots over to the caller, which must call release once.
func (s *slot) take() (release func()) {
	s.taken = true
	return s.free
}

func (s *slot) release() {
	if !s.taken {
		s.free()
	}
}

func (s *slot) free() {
	if s.heavy != nil {
		<-s.heavy
	}
	<-s.ch
}

// run dispatches the op and stores its output in res. ctx is the
// operation's context, with the soft deadline of a heavy one; wait is the
// probe's own context, until which a local operation's outcome is awaited
// (see runLocal). A panic in a prober becomes an error.
func (e *Executor) run(ctx, wait context.Context, op string, p *syncrv1.RunProbe, res *syncrv1.ProbeResult, s *slot) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("probe panicked", "probe_id", p.GetProbeId(), "op", op, "panic", fmt.Sprint(r))
			err = runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "the probe failed unexpectedly")
		}
	}()
	id, spec := p.GetProbeId(), p.GetSpec()
	remotes := spec.GetRemotes()
	switch op {
	case "list_local":
		o := spec.GetListLocal()
		l, err := runLocal(wait, e, s, func() (localfs.Listing, error) {
			return e.cfg.Local.List(ctx, o.GetPath(), e.entries(o.GetMaxEntries()), o.GetDirsOnly())
		})
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Listing{Listing: localListing(l)}
	case "list_remote":
		o := proto.CloneOf(spec.GetListRemote())
		o.MaxEntries = int32(e.entries(o.GetMaxEntries())) //nolint:gosec // bounded by MaxEntries
		l, err := e.cfg.Rclone.List(ctx, id, remotes, o)
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Listing{Listing: l}
	case "about":
		u, err := e.cfg.Rclone.About(ctx, id, remotes, spec.GetAbout())
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Usage{Usage: u}
	case "restic_snapshots":
		o := proto.CloneOf(spec.GetResticSnapshots())
		if o.GetMaxSnapshots() <= 0 || int(o.GetMaxSnapshots()) > e.cfg.MaxSnapshots {
			o.MaxSnapshots = int32(e.cfg.MaxSnapshots) //nolint:gosec // a small configured bound
		}
		s, err := e.cfg.Restic.Snapshots(ctx, id, remotes, o)
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Snapshots{Snapshots: s}
	case "restic_ls":
		o := proto.CloneOf(spec.GetResticLs())
		o.MaxEntries = int32(e.entries(o.GetMaxEntries())) //nolint:gosec // bounded by MaxEntries
		l, err := e.cfg.Restic.Ls(ctx, id, remotes, o)
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Listing{Listing: l}
	case "restic_init":
		c, err := e.cfg.Restic.Init(ctx, id, remotes, spec.GetResticInit())
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_RepositoryConfig{RepositoryConfig: c}
	case "restic_config":
		c, err := e.cfg.Restic.Config(ctx, id, remotes, spec.GetResticConfig())
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_RepositoryConfig{RepositoryConfig: c}
	case "sftp_host_keys":
		o := spec.GetSftpHostKeys()
		keys, err := e.cfg.HostKeys.HostKeys(ctx, o.GetHost(), int(o.GetPort()))
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_SftpHostKeys{SftpHostKeys: &syncrv1.SftpHostKeys{Keys: keys}}
	case "measure_local":
		o := spec.GetMeasureLocal()
		m, err := runLocal(wait, e, s, func() (localfs.Measurement, error) {
			return e.cfg.Local.Measure(ctx, o.GetPath(), e.cfg.MaxEntries)
		})
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Measurement{Measurement: localMeasurement(m)}
	case "measure_remote":
		m, err := e.cfg.Rclone.Measure(ctx, id, remotes, spec.GetMeasureRemote(), e.cfg.MaxEntries)
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Measurement{Measurement: e.capChildren(m)}
	case "restic_measure":
		m, err := e.cfg.Restic.Measure(ctx, id, remotes, spec.GetResticMeasure(), e.cfg.MaxEntries)
		if err != nil {
			return err
		}
		res.Output = &syncrv1.ProbeResult_Measurement{Measurement: e.capChildren(m)}
	case "restic_find":
		o := proto.CloneOf(spec.GetResticFind())
		o.MaxMatches = bound(o.GetMaxMatches(), DefaultMatches, MaxMatches)
		f, err := e.cfg.Restic.Find(ctx, id, remotes, o)
		if err != nil {
			return err
		}
		if n := int(o.GetMaxMatches()); len(f.GetMatches()) > n {
			f.Matches, f.Truncated = f.GetMatches()[:n], true
		}
		res.Output = &syncrv1.ProbeResult_Find{Find: f}
	case "restic_diff":
		o := proto.CloneOf(spec.GetResticDiff())
		o.MaxChanges = bound(o.GetMaxChanges(), DefaultChanges, MaxChanges)
		d, err := e.cfg.Restic.Diff(ctx, id, remotes, o)
		if err != nil {
			return err
		}
		if n := int(o.GetMaxChanges()); len(d.GetChanges()) > n {
			d.Changes, d.Truncated = d.GetChanges()[:n], true
		}
		res.Output = &syncrv1.ProbeResult_Diff{Diff: d}
	}
	return nil
}

// runLocal runs fn, a local filesystem operation, in a goroutine that
// takes over the probe's slots s, and returns its outcome, or wait's error
// as soon as wait (the probe's own context) ends: a filesystem call
// blocked in the kernel cannot be interrupted, so the operation may
// outlive the probe. fn gets its context itself: a measurement's ends at
// the soft deadline, earlier than wait, and fn then returns its partial
// result. The goroutine releases the slots when fn returns, before it
// hands over the outcome: a probe answered with fn's outcome has freed its
// slots, and one answered at its deadline frees them once fn notices its
// context or its stuck call returns. A panic becomes an error.
func runLocal[T any](wait context.Context, e *Executor, s *slot, fn func() (T, error)) (T, error) {
	type outcome struct {
		v   T
		err error
	}
	release := s.take()
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		func() {
			defer func() {
				if r := recover(); r != nil {
					e.log.Error("local probe panicked", "panic", fmt.Sprint(r))
					out.err = runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "the probe failed unexpectedly")
				}
			}()
			out.v, out.err = fn()
		}()
		release()
		done <- out
	}()
	select {
	case out := <-done:
		return out.v, out.err
	case <-wait.Done():
		var zero T
		return zero, wait.Err()
	}
}

// bound returns a heavy operation's result bound: the request's, def when
// it names none, never more than limit.
func bound(requested int32, def, limit int) int32 {
	n := int(requested)
	if n <= 0 {
		n = def
	}
	return int32(min(n, limit)) //nolint:gosec // at most limit, a small constant
}

// capChildren keeps at most MaxEntries child folders of a measurement
// (the probers return them largest first).
func (e *Executor) capChildren(m *syncrv1.ProbeMeasurement) *syncrv1.ProbeMeasurement {
	if len(m.GetChildren()) > e.cfg.MaxEntries {
		m.Children, m.ChildrenTruncated = m.GetChildren()[:e.cfg.MaxEntries], true
	}
	return m
}

// entries returns the bound of a listing: the request's, DefaultEntries
// when it names none, never more than MaxEntries.
func (e *Executor) entries(requested int32) int {
	n := int(requested)
	if n <= 0 {
		n = DefaultEntries
	}
	return min(n, e.cfg.MaxEntries)
}

// fit trims a result so its encoding stays within MaxResultBytes: the last
// entries of a folder listing, the oldest snapshots of a snapshot listing
// (they are ordered oldest first), the smallest child folders of a
// measurement (largest first), the last matches of a search (newest
// snapshot first) and the last changes of a comparison. What was trimmed
// is marked truncated (children_truncated for a measurement).
func (e *Executor) fit(res *syncrv1.ProbeResult) {
	limit := e.cfg.MaxResultBytes
	for proto.Size(res) > limit {
		switch out := res.GetOutput().(type) {
		case *syncrv1.ProbeResult_Listing:
			n := len(out.Listing.GetEntries())
			if n == 0 {
				return
			}
			out.Listing.Entries = out.Listing.GetEntries()[:n/2]
			out.Listing.Truncated = true
		case *syncrv1.ProbeResult_Snapshots:
			n := len(out.Snapshots.GetSnapshots())
			if n == 0 {
				return
			}
			out.Snapshots.Snapshots = out.Snapshots.GetSnapshots()[n-n/2:]
			out.Snapshots.Truncated = true
		case *syncrv1.ProbeResult_Measurement:
			n := len(out.Measurement.GetChildren())
			if n == 0 {
				return
			}
			out.Measurement.Children = out.Measurement.GetChildren()[:n/2]
			out.Measurement.ChildrenTruncated = true
		case *syncrv1.ProbeResult_Find:
			n := len(out.Find.GetMatches())
			if n == 0 {
				return
			}
			out.Find.Matches = out.Find.GetMatches()[:n/2]
			out.Find.Truncated = true
		case *syncrv1.ProbeResult_Diff:
			n := len(out.Diff.GetChanges())
			if n == 0 {
				return
			}
			out.Diff.Changes = out.Diff.GetChanges()[:n/2]
			out.Diff.Truncated = true
		default:
			return
		}
	}
}

// localListing converts a local listing to its wire form.
func localListing(l localfs.Listing) *syncrv1.ProbeListing {
	out := &syncrv1.ProbeListing{Path: l.Path, Truncated: l.Truncated, Entries: make([]*syncrv1.ProbeEntry, 0, len(l.Entries))}
	for _, e := range l.Entries {
		pe := &syncrv1.ProbeEntry{
			Name: runner.ValidText(e.Name),
			Path: runner.ValidText(e.Path),
			Size: e.Size,
			Mode: e.Mode.String(),
		}
		switch {
		case e.Dir:
			// A link to a folder is listed as a folder: it can be opened
			// (when its target is inside the roots).
			pe.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
		case e.Symlink:
			pe.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK
		case e.Mode.IsRegular():
			pe.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE
		default:
			pe.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER
		}
		if !e.ModTime.IsZero() {
			pe.ModTime = timestamppb.New(e.ModTime)
		}
		out.Entries = append(out.Entries, pe)
	}
	return out
}

// localMeasurement converts a local measurement to its wire form.
func localMeasurement(m localfs.Measurement) *syncrv1.ProbeMeasurement {
	out := &syncrv1.ProbeMeasurement{
		Path:              runner.ValidText(m.Path),
		Bytes:             m.Bytes,
		Files:             m.Files,
		Dirs:              m.Dirs,
		Children:          make([]*syncrv1.ProbeChildMeasurement, 0, len(m.Children)),
		ChildrenTruncated: m.ChildrenTruncated,
		Complete:          m.Complete,
		Unreadable:        m.Unreadable,
	}
	for _, c := range m.Children {
		out.Children = append(out.Children, &syncrv1.ProbeChildMeasurement{
			Name: runner.ValidText(c.Name), Bytes: c.Bytes, Files: c.Files, Dirs: c.Dirs,
		})
	}
	return out
}

// codeOf maps a prober's error to its wire code: a runner error keeps its
// code; the path policy's and the filesystem's errors get theirs.
func codeOf(err error) syncrv1.JobErrorCode {
	if _, ok := errors.AsType[*runner.Error](err); ok {
		return runner.CodeOf(err)
	}
	switch {
	case errors.Is(err, localfs.ErrNotAllowed):
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED
	case errors.Is(err, localfs.ErrInvalidPath):
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC
	case errors.Is(err, fs.ErrNotExist):
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND
	case errors.Is(err, fs.ErrPermission), errors.Is(err, localfs.ErrUnresolvable):
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
	}
	return runner.CodeOf(err)
}

// clip shortens s to at most n bytes on a UTF-8 boundary, after making it
// valid UTF-8.
func clip(s string, n int) string {
	s = runner.ValidText(s)
	if len(s) <= n {
		return s
	}
	cut := n - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
