package rclone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/bisyncstate"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// BisyncKind is the job kind BisyncRunner executes.
const BisyncKind = "rclone_bisync"

// BisyncRunner runs two-way syncs ("rclone_bisync", rclone bisync) between
// two locations, each a remote of the spec or a local path inside the
// worker's local_roots (see docs/adr/0018-two-way-sync.md).
//
// It shares the transfer runner's isolation (a scratch directory per job
// with an empty rclone config), remote environment, location checks,
// progress tracker and failure messages. Unlike a transfer, a two-way sync
// has state: the pair's directory in the state store (bisyncstate), named
// by the spec's state key, holds rclone's work directory and the filters
// file. A run:
//
//  1. checks the spec: the state key's format, the options, both
//     locations (neither may be the state store's folder, inside it or
//     hold it), and the two rclone paths together at most 200 bytes;
//  2. opens the pair (its lock; TEMPORARY if another run holds it) and
//     removes rclone lock files a killed run left;
//  3. resyncs when the spec asks (reason REQUESTED) or the pair has no
//     baseline (Pair.Fresh; reason FIRST_RUN), with --resync-mode. Before
//     a resync it checks the local sides, which rclone does not: one that
//     does not exist is NOT_FOUND, and an automatic first run never fills
//     an empty one (RESYNC_REQUIRED, see checkResyncSides);
//  4. writes the filters file and runs rclone bisync;
//  5. reports the conflicts (output and a warning), or maps rclone's
//     final messages to an error code: RESYNC_REQUIRED, SAFETY_ABORT,
//     SIDE_UNAVAILABLE, NOT_FOUND, else the exit code as for transfers.
//     After a critical error that rclone says needs a resync, it reads the
//     pair's state: when the next run can go on from the listings rclone
//     kept (--recover), the job fails with EXECUTION_FAILED and says to fix
//     the cause and run again; only otherwise with RESYNC_REQUIRED.
//
// A cancel, time limit or shutdown stops rclone as for transfers; the next
// run removes the lock file a kill leaves and rclone's --recover finishes
// without a resync.
type BisyncRunner struct {
	r      *Runner
	states *bisyncstate.Store
}

var _ runner.Runner = (*BisyncRunner)(nil)

// NewBisync returns a BisyncRunner with the transfer runner's dependencies
// and the pairs' state store, or an error if one is missing.
func NewBisync(cfg Config, states *bisyncstate.Store) (*BisyncRunner, error) {
	r, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if states == nil {
		return nil, errors.New("rclone runner: two-way sync state store is nil")
	}
	return &BisyncRunner{r: r, states: states}, nil
}

// Kind implements runner.Runner.
func (*BisyncRunner) Kind() string { return BisyncKind }

// Run implements runner.Runner.
func (b *BisyncRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	spec := job.Spec.GetRcloneBisync()
	if spec == nil {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "job spec is not an rclone two-way sync")
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("rclone: not started: %w", context.Cause(ctx))
	}
	if !bisyncstate.ValidKey(spec.GetStateKey()) {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, bisyncstate.ErrInvalidKey)
	}
	opts := spec.GetOptions()
	if _, err := bisyncOptionFlags(opts, false); err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, err)
	}
	filters, err := filterLines(opts.GetFilters())
	if err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, err)
	}

	dir, err := b.r.scratchDir(job.ID)
	if err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, err)
	}
	defer b.r.removeScratch(dir)
	env, err := b.r.cfg.Env.Build(dir, job.Spec.GetRemotes())
	if err != nil {
		code := syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC
		if errors.Is(err, rcloneenv.ErrLocalPath) {
			code = syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED
		}
		return res, runner.Fail(code, err)
	}
	path1, err := b.r.location("path1", spec.GetPath1(), env)
	if err != nil {
		return res, err
	}
	path2, err := b.r.location("path2", spec.GetPath2(), env)
	if err != nil {
		return res, err
	}
	if err := checkPair(path1, path2); err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, err)
	}
	for _, side := range [...][2]string{{"path1", path1}, {"path2", path2}} {
		if err := b.outsideState(side[0], side[1]); err != nil {
			return res, err
		}
	}

	pair, err := b.states.Open(spec.GetStateKey())
	switch {
	case errors.Is(err, bisyncstate.ErrBusy):
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, err)
	case err != nil:
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "open the two-way sync's state: %w", err)
	}
	defer func() {
		if err := pair.Close(); err != nil {
			b.r.cfg.Logger.Warn("cannot release the lock of a two-way sync's state", "job_id", job.ID, "error", err.Error())
		}
	}()
	reason, err := b.prepare(pair, spec, filters, job.ID)
	if err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, err)
	}
	if err := checkResyncSides(reason, path1, path2); err != nil {
		return res, err
	}
	args, err := bisyncArgs(opts, bisyncState{
		workDir:     pair.WorkDir(),
		filtersFile: pair.FiltersFile(),
		resync:      reason != syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED,
		force:       spec.GetForce(),
	}, path1, path2)
	if err != nil {
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, err)
	}

	sc := scrub.New(env.Secrets())
	t := newTracker(emit, sc)
	log := &bisyncLog{sc: sc, stateDir: pair.Dir()}
	t.observe = log.line
	pres, err := b.r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path:   b.r.cfg.Binary,
		Args:   args,
		Dir:    dir,
		Env:    env.Vars(),
		Stdout: t.line,
		Stderr: t.line,
	})
	res.ExitCode = pres.ExitCode
	res.Stats = t.progress
	res.Stats.Elapsed = max(res.Stats.Elapsed, pres.Duration)
	if err := b.outcome(err, pres, t, log, reason, pair, opts.GetDryRun()); err != nil {
		return res, err
	}
	res.Output = &syncrv1.JobOutput{Output: &syncrv1.JobOutput_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncOutput{
		ResyncReason:  reason,
		Conflicts:     log.conflicts,
		ConflictPaths: log.conflictPaths,
		DryRun:        opts.GetDryRun(),
	}}}
	if w := conflictWarning(log.conflicts, opts); w != "" {
		res.Warnings = runner.LimitWarnings([]string{w})
	}
	return res, nil
}

// prepare readies the open pair for rclone: it removes the lock files of
// killed runs, decides whether the run resyncs and why, and writes the
// filters file.
func (b *BisyncRunner) prepare(pair *bisyncstate.Pair, spec *syncrv1.RcloneBisync, filters []string, jobID string) (syncrv1.BisyncResyncReason, error) {
	reason := syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED
	n, err := pair.ClearStaleLocks()
	if err != nil {
		return reason, err
	}
	if n > 0 {
		b.r.cfg.Logger.Info("removed the rclone lock file of an interrupted two-way sync run", "job_id", jobID, "files", n)
	}
	fresh, err := pair.Fresh()
	switch {
	case err != nil:
		return reason, err
	case spec.GetResync():
		reason = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED
	case fresh:
		reason = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN
	}
	return reason, pair.WriteFilters(filters)
}

// outcome maps the supervisor's result to the runner's error: rclone's
// final messages first (bisyncLog.failure, told what the pair's state now
// allows), then as for transfers.
func (b *BisyncRunner) outcome(err error, pres proc.Result, t *tracker, log *bisyncLog,
	reason syncrv1.BisyncResyncReason, pair *bisyncstate.Pair, dryRun bool,
) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*proc.ExitError](err); ok {
		run := failedRun{
			next:   b.next(pair),
			resync: reason != syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED,
			dryRun: dryRun,
		}
		if code, msg, ok := log.failure(run); ok {
			return runner.Fail(code, &exitError{msg: msg, err: err})
		}
	}
	cmd := "bisync"
	switch reason {
	case syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN:
		cmd = "bisync --resync (first run of the pair on this worker)"
	case syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED:
		cmd = "bisync --resync"
	}
	return b.r.outcome(err, pres, t, cmd)
}

// next reads, after a failed run, what the pair's state lets its next run
// do; when the work directory cannot be read, it assumes only a resync
// helps.
func (b *BisyncRunner) next(pair *bisyncstate.Pair) pairNext {
	fresh, err := pair.Fresh()
	if err == nil && fresh {
		return nextResyncs
	}
	var resumable bool
	if err == nil {
		resumable, err = pair.Resumable()
	}
	if err != nil {
		b.r.cfg.Logger.Warn("cannot read the state of a two-way sync after a failed run", "error", err.Error())
	}
	if resumable {
		return nextContinues
	}
	return nextNeedsResync
}

// outsideState refuses (PATH_NOT_ALLOWED) a local side that is the folder
// of the two-way sync state store, is inside it, or holds it, also through
// a symbolic link: a run there would sync rclone's own listings, and let
// the job read or change the state of other pairs. local_roots may cover
// the worker's data directory; this keeps the pairs' state out of reach
// anyway. path is a side's rclone path; remotes are not checked.
func (b *BisyncRunner) outsideState(role, path string) error {
	if !filepath.IsAbs(path) {
		return nil
	}
	overlap, err := localfs.Overlap(path, b.states.Root())
	if err != nil {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "%s: %w", role, err)
	}
	if overlap {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED,
			"%s: %s is, holds or is inside the folder where the worker keeps the state of two-way syncs (%s)", role, path, b.states.Root())
	}
	return nil
}

// checkResyncSides checks the local sides of a resync (reason set) before
// rclone runs, because rclone checks them only in a normal run: during
// --resync it creates a side that does not exist and fills an empty one.
//
//   - A local side that does not exist fails the run with NOT_FOUND, as in
//     a normal run.
//   - An automatic first run (FIRST_RUN) never fills an empty local side:
//     it fails with RESYNC_REQUIRED, without running rclone. An empty
//     mount point (a drive that is not mounted) looks the same as a new
//     folder; filled from the other side and recorded as the pair's
//     baseline, it would make every file of the drive look deleted once the
//     drive is mounted again, and the next normal run would delete those
//     files on the other side. A resync someone asked for (REQUESTED)
//     fills it; the UI warns about drives first.
//
// Empty means no entry at all, hidden files included; the filters are not
// applied, so a folder that holds only excluded files is not empty. When
// every side is local and empty there is nothing to fill, and the run goes
// ahead. A side that cannot be read is left to rclone.
func checkResyncSides(reason syncrv1.BisyncResyncReason, path1, path2 string) error {
	if reason == syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED {
		return nil
	}
	sides := [...]struct {
		name, path string
		local      bool
		empty      bool
	}{{name: "Path1", path: path1}, {name: "Path2", path: path2}}
	for i := range sides {
		s := &sides[i]
		if s.local = filepath.IsAbs(s.path); !s.local {
			continue
		}
		empty, err := emptyDir(s.path)
		if errors.Is(err, fs.ErrNotExist) {
			return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
				"%s does not exist: %s; a resync would create it and fill it from the other side, "+
					"so create the folder (or mount its drive) first", sideName(s.name), s.path)
		}
		s.empty = err == nil && empty
	}
	if reason != syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN ||
		(sides[0].local && sides[0].empty && sides[1].local && sides[1].empty) {
		return nil
	}
	for i, s := range sides {
		if s.empty {
			other := sideName(sides[1-i].name)
			return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED,
				"%s is empty; a first run does not fill an empty folder on its own (a drive that is not mounted looks the same). "+
					"If this is a new machine, run it once with resync to fill it from %s; if the folder is on a drive, mount it first",
				sideName(s.name), other)
		}
	}
	return nil
}

// emptyDir reports whether the directory path has no entries.
func emptyDir(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // a side the path policy accepted
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.ReadDir(1); errors.Is(err, io.EOF) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// checkPair refuses two rclone paths that are too long together for
// rclone's state file names, equal, or nested (a two-way sync of a folder
// with a folder inside it would sync it into itself). The controller
// checks the same; this guards against a spec it did not build.
func checkPair(path1, path2 string) error {
	if n := len(path1) + len(path2); n > maxBisyncPathBytes {
		return fmt.Errorf("the two paths together are too long for a two-way sync (rclone keeps its state in files named after them): %d bytes, at most %d", n, maxBisyncPathBytes)
	}
	if nested(path1, path2) || nested(path2, path1) {
		return errors.New("path1 and path2 are the same location or one is inside the other")
	}
	return nil
}

// nested reports whether rclone path inner is outer or inside it: two
// local paths (absolute, cleaned by the path policy) or two paths on the
// same remote alias. Paths on different remotes are never nested here.
func nested(outer, inner string) bool {
	if filepath.IsAbs(outer) != filepath.IsAbs(inner) {
		return false
	}
	if filepath.IsAbs(outer) {
		rel, err := filepath.Rel(outer, inner)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	oa, op, _ := strings.Cut(outer, ":")
	ia, ip, _ := strings.Cut(inner, ":")
	if oa != ia {
		return false
	}
	op, ip = strings.Trim(op, "/"), strings.Trim(ip, "/")
	return op == "" || ip == op || strings.HasPrefix(ip, op+"/")
}

// conflictWarning returns the warning of a run with n conflicts, or "".
func conflictWarning(n int64, opts *syncrv1.RcloneBisyncOptions) string {
	if n == 0 {
		return ""
	}
	files := "1 file"
	if n > 1 {
		files = fmt.Sprintf("%d files", n)
	}
	if opts.GetDryRun() {
		return files + " changed on both sides (dry run: nothing was changed)"
	}
	suffix := opts.GetConflictSuffix()
	if suffix == "" {
		suffix = "conflict"
	}
	winner, _ := conflictResolve(opts.GetConflictResolve())
	if winner == "none" {
		return fmt.Sprintf("%s changed on both sides; both versions were kept, renamed with the suffix %q", files, suffix)
	}
	if side := sideName(strings.Replace(winner, "path", "Path", 1)); side != "" {
		winner = side + "'s"
	}
	return fmt.Sprintf("%s changed on both sides; the %s version kept the name where that could be decided, "+
		"and the other version was kept, renamed with the suffix %q", files, winner, suffix)
}
