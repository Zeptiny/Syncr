package jobkind

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Limits of RcloneBisyncDefinition beyond those of transfers. They mirror
// the worker's two-way sync runner (internal/worker/runner/rclone,
// bisync*.go), which enforces them again.
const (
	// maxBisyncPathBytes bounds the two sides' paths together: rclone
	// names its state files after both full rclone paths, and a file name
	// over 255 bytes makes it fail with a misleading "prior lock file
	// found". 200 leaves room for the joiner and the suffixes.
	maxBisyncPathBytes = 200
	// minMaxDeletePercent and maxMaxDeletePercent bound the delete limit
	// (max_delete_percent; unset means 50).
	minMaxDeletePercent = 1
	maxMaxDeletePercent = 100
)

// bisyncField is the field path of the two-way sync in a definition.
const bisyncField = "definition.rclone_bisync"

// conflictSuffixRe matches RcloneBisyncOptions.conflict_suffix: one suffix
// for both sides, or Path1's and Path2's separated by a comma.
var conflictSuffixRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}(,[A-Za-z0-9._-]{1,32})?$`)

// Messages of the two-way sync's checks.
const (
	// bisyncTooLong is the message of two sides whose paths are too long
	// together, here and (with the byte counts) on the worker.
	bisyncTooLong = "the two paths together are too long for a two-way sync (rclone keeps its state in files named after them)"
	// bisyncNested is the message of equal or nested sides.
	bisyncNested = "must differ from path1, and neither may be inside the other"
	// oneShotMessage is the message of a one-shot field in a schedule.
	oneShotMessage = "can only be set for a single run"
	// singleWorkerReason is why a two-way sync's schedule cannot target a
	// label.
	singleWorkerReason = "a two-way sync keeps its state on one worker; choose a worker"
)

// Bisync is the "rclone_bisync" kind ("Two-way sync"): rclone bisync keeps
// two locations, each on one of the owner's remotes or on the worker's
// filesystem, in step in both directions (docs/adr/0018-two-way-sync.md).
//
// A pair's state (rclone's listings) stays on the worker that ran it, in a
// directory named by the spec's state key (bisyncStateKey), so its
// schedules target one worker (SingleWorker). Its jobs hold one
// concurrency key per side (BisyncLocationKey), so runs that share a
// location, on any worker, run one at a time. resync and force are for a
// single run (OneShot). Workers need link.FeatureRcloneBisync.
type Bisync struct{}

var (
	_ Kind         = Bisync{}
	_ Keyed        = Bisync{}
	_ OneShot      = Bisync{}
	_ SingleWorker = Bisync{}
	_ Requirer     = Bisync{}
	_ Labeled      = Bisync{}
	_ Summarizer   = Bisync{}
)

// Name implements Kind.
func (Bisync) Name() string { return "rclone_bisync" }

// Label implements Labeled.
func (Bisync) Label() string { return "Two-way sync" }

// RequiredFeatures implements Requirer: workers that run rclone bisync.
func (Bisync) RequiredFeatures() []string { return []string{link.FeatureRcloneBisync} }

// SingleWorker implements SingleWorker.
func (Bisync) SingleWorker() string { return singleWorkerReason }

// OneShotFields implements OneShot: resync and force are for a single run
// (after RESYNC_REQUIRED or SAFETY_ABORT); a schedule would repeat them.
func (Bisync) OneShotFields(def *syncrv1.JobDefinition) []domain.FieldError {
	b := def.GetRcloneBisync()
	var out []domain.FieldError
	if b.GetResync() {
		out = append(out, domain.FieldError{Field: bisyncField + ".resync", Message: oneShotMessage})
	}
	if b.GetForce() {
		out = append(out, domain.FieldError{Field: bisyncField + ".force", Message: oneShotMessage})
	}
	return out
}

// Check implements Kind. It checks both locations as transfers do (a
// valid remote ID and a relative path without ".." segments, or an
// absolute local path), that neither is the other or inside it (on the
// same remote, or both on the worker), that their paths are not too long
// together (a necessary condition: Build checks the full rclone paths),
// and the options (ranges, known enum values, the conflict suffix, filter
// rules). Refs are both remotes, deduplicated; the search terms are
// "bisync", "two-way" and both paths.
func (Bisync) Check(def *syncrv1.JobDefinition) (Refs, error) {
	b := def.GetRcloneBisync()
	var v domain.Validation
	if b == nil {
		v.Add(bisyncField, "is required")
		return Refs{}, v.Err()
	}
	p1 := checkLocation(&v, bisyncField+".path1", b.GetPath1())
	p2 := checkLocation(&v, bisyncField+".path2", b.GetPath2())
	if p1.ok && p2.ok {
		if p1.remote == p2.remote && (keyNested(p1, p2) || keyNested(p2, p1)) {
			v.Add(bisyncField+".path2", bisyncNested)
		}
		if n := len(p1.key) + len(p2.key); n > maxBisyncPathBytes {
			v.Add(bisyncField+".path1.path", fmt.Sprintf("%s: %d bytes, at most %d", bisyncTooLong, n, maxBisyncPathBytes))
		}
	}
	checkBisyncOptions(&v, bisyncField+".options", b.GetOptions())
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	var refs Refs
	for _, l := range []location{p1, p2} {
		if !l.remote.IsZero() && !slices.Contains(refs.RemoteIDs, l.remote) {
			refs.RemoteIDs = append(refs.RemoteIDs, l.remote)
		}
	}
	refs.SearchTerms = []string{"bisync", "two-way"}
	for _, p := range []string{p1.path, p2.path} {
		if p != "" {
			refs.SearchTerms = append(refs.SearchTerms, p)
		}
	}
	return refs, nil
}

// keyNested reports whether checked location inner is outer or inside it,
// by their normalized paths (location.key); both are on the same remote,
// or both on the worker. On a remote, a leading "/" is ignored (the
// worker compares them so); on the worker, Windows paths compare without
// regard to case, as Windows does.
func keyNested(outer, inner location) bool {
	if !outer.remote.IsZero() {
		return pathWithin(strings.Trim(outer.key, "/"), strings.Trim(inner.key, "/"), "/", false)
	}
	return localNested(outer.key, inner.key)
}

// localNested reports whether the cleaned local path inner (backend.JoinLocal)
// is outer or inside it. A POSIX and a Windows path are never nested.
func localNested(outer, inner string) bool {
	ow, iw := absoluteWindows(outer), absoluteWindows(inner)
	if ow != iw {
		return false
	}
	if ow {
		return pathWithin(outer, inner, `\`, true)
	}
	return pathWithin(outer, inner, "/", false)
}

// absoluteWindows reports whether p is a Windows path (a drive letter or a
// UNC path); absoluteLocal accepted it.
func absoluteWindows(p string) bool { return absoluteLocal(p) && !strings.HasPrefix(p, "/") }

// pathWithin reports whether inner is outer or below it, with segments
// separated by sep; "" (a remote's root) contains everything. fold
// compares without regard to case.
func pathWithin(outer, inner, sep string, fold bool) bool {
	if fold {
		outer, inner = strings.ToLower(outer), strings.ToLower(inner)
	}
	if outer == "" || outer == inner {
		return true
	}
	if !strings.HasSuffix(outer, sep) {
		outer += sep
	}
	return strings.HasPrefix(inner, outer)
}

// checkBisyncOptions checks the options like the worker's runner does;
// nil options are Syncr's defaults.
func checkBisyncOptions(v *domain.Validation, field string, o *syncrv1.RcloneBisyncOptions) {
	if o == nil {
		return
	}
	intRange := func(name string, p *int32, lo, hi int32) {
		if p != nil && (*p < lo || *p > hi) {
			v.Add(field+"."+name, fmt.Sprintf("must be between %d and %d", lo, hi))
		}
	}
	intRange("transfers", o.Transfers, 1, maxTransfers)
	intRange("checkers", o.Checkers, 1, maxCheckers)
	intRange("low_level_retries", o.LowLevelRetries, 0, maxLowLevelRetries)
	intRange("max_delete_percent", o.MaxDeletePercent, minMaxDeletePercent, maxMaxDeletePercent)
	if p := o.BandwidthLimitBytesPerSecond; p != nil && *p < 0 {
		v.Add(field+".bandwidth_limit_bytes_per_second", "must not be negative")
	}
	seen := make(map[syncrv1.BisyncCompare]bool, len(o.GetCompare()))
	for i, c := range o.GetCompare() {
		switch {
		case c != syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE && c != syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME &&
			c != syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM:
			v.Add(fmt.Sprintf("%s.compare[%d]", field, i), "must be size, modtime or checksum")
		case seen[c]:
			v.Add(fmt.Sprintf("%s.compare[%d]", field, i), "is listed more than once")
		}
		seen[c] = true
	}
	if _, ok := syncrv1.BisyncConflictResolve_name[int32(o.GetConflictResolve())]; !ok {
		v.Add(field+".conflict_resolve", "is not a known conflict resolution")
	}
	// rclone's "delete" has no value: a conflict never loses a version.
	if _, ok := syncrv1.BisyncConflictLoser_name[int32(o.GetConflictLoser())]; !ok {
		v.Add(field+".conflict_loser", "must be num or pathname")
	}
	if _, ok := syncrv1.BisyncResyncMode_name[int32(o.GetResyncMode())]; !ok {
		v.Add(field+".resync_mode", "is not a known resync mode")
	}
	if s := o.GetConflictSuffix(); s != "" && !conflictSuffixRe.MatchString(s) {
		v.Add(field+".conflict_suffix", `must be one suffix, or two separated by a comma, each 1 to 32 letters, digits, ".", "_" or "-"`)
	}
	checkFilterRules(v, field, o.GetFilters())
}

// Build implements Kind: the two-way sync with its two locations on the
// aliased remotes (see the package documentation), the options, the
// one-shot fields and the pair's state key. It refuses (on
// definition.rclone_bisync.path1.path) two sides whose rclone paths, as
// the worker passes them ("r1:<root/path>" or a cleaned local path), are
// over 200 bytes together, and (on definition.rclone_bisync.path2) sides
// that turn out to be the same or nested once their remotes are known (a
// local remote and a worker path). It uses in.Remotes only.
func (k Bisync) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	// Stored definitions are checked again: a spec is never built from a
	// definition the current rules reject.
	refs, err := k.Check(def)
	if err != nil {
		return nil, err
	}
	b := def.GetRcloneBisync()
	rs, err := newSpecRemotes(refs.RemoteIDs, in.Remotes)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	var sides [2]bisyncSide
	for i, l := range []*syncrv1.JobLocation{b.GetPath1(), b.GetPath2()} {
		field := fmt.Sprintf("%s.path%d", bisyncField, i+1)
		loc, err := rs.location(field, l)
		if err != nil {
			return nil, fmt.Errorf("build %s: %w", k.Name(), err)
		}
		sides[i] = bisyncSide{loc: loc}
		if raw := l.GetRemoteId(); raw != "" {
			// Check parsed it.
			sides[i].remote, _ = domain.ParseRemoteID(raw)
		}
	}
	p1, p2 := rclonePath(sides[0].loc), rclonePath(sides[1].loc)
	if n := len(p1) + len(p2); n > maxBisyncPathBytes {
		return nil, fmt.Errorf("build %s: %w", k.Name(), invalid(bisyncField+".path1.path",
			fmt.Sprintf("%s: %d bytes with the remotes' roots, at most %d", bisyncTooLong, n, maxBisyncPathBytes)))
	}
	if specNested(sides[0].loc, sides[1].loc) || specNested(sides[1].loc, sides[0].loc) {
		return nil, fmt.Errorf("build %s: %w", k.Name(), invalid(bisyncField+".path2", bisyncNested))
	}
	key, err := rs.bisyncStateKey(sides)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	defs, err := rs.definitions()
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	op := &syncrv1.RcloneBisync{
		Path1: sides[0].loc, Path2: sides[1].loc, Resync: b.GetResync(), Force: b.GetForce(), StateKey: key,
	}
	if opts := b.GetOptions(); opts != nil {
		op.Options = proto.CloneOf(opts)
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_RcloneBisync{RcloneBisync: op}}, nil
}

// rclonePath is the path rclone gets for a spec location, as the worker
// passes it: "alias:path" on a remote (rcloneenv.RemotePath), or the local
// path, which backend.JoinLocal already cleaned (the worker's
// filepath.Clean keeps its length).
func rclonePath(loc *syncrv1.Location) string {
	if loc.GetRemote() == "" {
		return loc.GetPath()
	}
	return loc.GetRemote() + ":" + loc.GetPath()
}

// specNested reports whether spec location inner is outer or inside it,
// as the worker's runner judges it: two local paths, or two paths on the
// same remote alias (a leading or trailing "/" ignored). Paths on
// different remotes are never nested here.
func specNested(outer, inner *syncrv1.Location) bool {
	if outer.GetRemote() != inner.GetRemote() {
		return false
	}
	if outer.GetRemote() == "" {
		return localNested(outer.GetPath(), inner.GetPath())
	}
	return pathWithin(strings.Trim(outer.GetPath(), "/"), strings.Trim(inner.GetPath(), "/"), "/", false)
}

// ConcurrencyKeys implements Keyed: one key per side (BisyncLocationKey),
// so two pairs that share a location (a desktop's and a laptop's folder
// with the same remote path) and two jobs of one pair run one at a time,
// on any worker. Nil for a definition Check rejects.
func (Bisync) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	b := def.GetRcloneBisync()
	var v domain.Validation
	p1 := checkLocation(&v, bisyncField+".path1", b.GetPath1())
	p2 := checkLocation(&v, bisyncField+".path2", b.GetPath2())
	if !p1.ok || !p2.ok {
		return nil
	}
	return []string{bisyncLocationKey(p1), bisyncLocationKey(p2)}
}

// Describe implements Kind: "Two-way sync", then both sides, e.g. "Two-way
// sync /home/you/Shared ⇄ R2 offsite", with " · Resync" or " · Delete
// limit ignored" for a run that sets resync or force. A location on a
// remote is the remote's name, one on the worker's filesystem its path. It
// uses names.Remotes only.
func (Bisync) Describe(def *syncrv1.JobDefinition, names Names) string {
	b := def.GetRcloneBisync()
	out := "Two-way sync " + describeLocation(b.GetPath1(), names.Remotes) + " ⇄ " + describeLocation(b.GetPath2(), names.Remotes)
	if b.GetResync() {
		out += " · Resync"
	}
	if b.GetForce() {
		out += " · Delete limit ignored"
	}
	return out
}

// Summarize implements Summarizer: why the run was a resync, the
// conflicts (not for a resync without any: a resync has none to find), and
// that it was a dry run.
func (Bisync) Summarize(_ *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail {
	o := out.GetRcloneBisync()
	if o == nil {
		return nil
	}
	var ds []Detail
	switch o.GetResyncReason() {
	case syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN:
		ds = append(ds, Detail{"Resync", "First run"})
	case syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED:
		ds = append(ds, Detail{"Resync", "Requested"})
	}
	switch n := o.GetConflicts(); {
	case n > 0:
		ds = append(ds, Detail{"Conflicts", formatCount(n)})
	case o.GetResyncReason() == syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED:
		ds = append(ds, Detail{"Conflicts", "None"})
	}
	if o.GetDryRun() {
		ds = append(ds, Detail{"Dry run", "Nothing was changed"})
	}
	return ds
}
