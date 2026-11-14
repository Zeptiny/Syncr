package restic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/mount"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclonetool"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
	"github.com/Zeptiny/Syncr/internal/worker/sourcestate"
)

// RemoteSources are what backups of a folder of a remote
// (ResticBackup.remote_source) need on the worker.
type RemoteSources struct {
	// Store keeps the mirrors and mount points (<data_dir>/sources). Nil
	// makes every backup of a remote fail with TOOL_UNAVAILABLE.
	Store *sourcestate.Store
	// Mounter mounts remotes. Nil makes the mount method unavailable.
	Mounter Mounter
	// Support says which methods this worker can use, as its Hello
	// reports them; a method it cannot use fails with TOOL_UNAVAILABLE and
	// the reason. Nil: neither.
	Support *syncrv1.RemoteSourceSupport
	// MountTimeout bounds the wait until a mount is ready
	// (remote_sources.mount_timeout).
	MountTimeout time.Duration
}

// Mounter mounts a folder of a remote read-only for one backup;
// cmd/syncr-worker adapts *mount.Mounter to it.
type Mounter interface {
	// Mount returns once the folder is mounted at req.Point, or an error
	// (*mount.Error, or the context's cause once it ended).
	Mount(ctx context.Context, req mount.Request) (Mounted, error)
}

// Mounted is a mounted folder.
type Mounted interface {
	// Unmount stops the mount and waits until it has ended.
	Unmount() error
}

// filtersFileName is the rclone filters file of a backup of a remote, in
// the job's scratch directory.
const filtersFileName = "filters.txt"

// emptySnapshot is in restic's message when it refuses to save a snapshot
// of a folder without any entry ("Fatal: unable to save snapshot: snapshot
// is empty", restic 0.19.1, exit code 1).
const emptySnapshot = "snapshot is empty"

// backupRunner runs restic_backup: a backup of paths exactly as every other
// kind runs (kindRunner), or, with ResticBackup.remote_source, a backup of
// a folder of a remote (runRemoteSource).
type backupRunner struct{ *kindRunner }

// Run implements runner.Runner.
func (r *backupRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	if job.Spec.GetResticBackup().GetRemoteSource() == nil {
		return r.kindRunner.Run(ctx, job, emit)
	}
	return r.e.runRemoteSource(ctx, job, emit)
}

// remotePlan is a checked backup of a remote: the rclone path of the
// folder, the filter lines, the flags of the sync or mount, and the restic
// command that backs up ".".
type remotePlan struct {
	src *syncrv1.ResticBackupRemoteSource
	// target is the folder as an rclone path, "<alias>:<path>".
	target string
	// filters are the rclone filter lines (rclonetool.FilterLines).
	filters []string
	// syncFlags are the mirror's options (transfers, bwlimit, fast-list);
	// mountFlags the mount's (bwlimit).
	syncFlags, mountFlags []string
	// backup is restic's command, run with the mirror or the mount as
	// its working directory.
	backup command
}

// planRemoteSource checks a backup of a remote and plans it. Every problem
// is INVALID_SPEC, found before the worker touches its data directory:
// paths, excludes, use_fs_snapshot and one_file_system set with the remote
// source; an alias that is malformed or not in the spec's remotes; a path
// with a control character or a ".." segment; an unspecified method; a
// malformed source key; invalid filters; the mirror's options with the
// mount method; options out of range; and a folder that holds the
// repository or lies inside it (same remote).
func planRemoteSource(spec *syncrv1.JobSpec) (remotePlan, error) {
	op := spec.GetResticBackup()
	src := op.GetRemoteSource()
	p := remotePlan{src: src}
	switch {
	case len(op.GetPaths()) > 0:
		return p, invalid("a backup of a remote folder has no paths")
	case len(op.GetExcludes()) > 0:
		return p, invalid("a backup of a remote folder leaves files out with filters, not excludes")
	case op.GetUseFsSnapshot():
		return p, invalid("use_fs_snapshot (VSS) is not available for a backup of a remote folder")
	case op.GetOneFileSystem():
		return p, invalid("one_file_system is not available for a backup of a remote folder")
	}

	alias := src.GetRemote()
	switch {
	case alias == "":
		return p, invalid("the remote of the folder to back up is missing")
	case !aliasPattern.MatchString(alias):
		return p, invalid("invalid remote alias %q", alias)
	case !hasRemote(spec.GetRemotes(), alias):
		return p, invalid("the remote %q of the folder to back up is not defined in the job", alias)
	}
	path := src.GetPath()
	if err := checkSourcePath(path); err != nil {
		return p, err
	}
	p.target = alias + ":" + path

	mirror := false
	switch src.GetMethod() {
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR:
		mirror = true
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT:
	default:
		return p, invalid("the method of reading the remote folder must be mirror or mount")
	}
	if !link.ValidSourceKey(src.GetSourceKey()) {
		return p, invalid("source_key must be %d lower-case hex digits", link.SourceKeyLen)
	}
	filters, err := rclonetool.FilterLines(src.GetFilters())
	if err != nil {
		return p, invalid("filters: %w", err)
	}
	p.filters = filters
	if !mirror {
		switch {
		case src.GetDiscardMirror():
			return p, invalid("discard_mirror is only available with the mirror method")
		case src.Transfers != nil:
			return p, invalid("transfers is only available with the mirror method")
		case src.GetFastList():
			return p, invalid("fast_list is only available with the mirror method")
		}
	}
	transfers, err := rclonetool.TransfersFlag("transfers", src.Transfers)
	if err != nil {
		return p, invalid("%w", err)
	}
	bwlimit, err := rclonetool.BandwidthFlag("bandwidth_limit_bytes_per_second", src.BandwidthLimitBytesPerSecond)
	if err != nil {
		return p, invalid("%w", err)
	}
	for _, f := range []string{transfers, bwlimit} {
		if f != "" {
			p.syncFlags = append(p.syncFlags, f)
		}
	}
	if src.GetFastList() {
		p.syncFlags = append(p.syncFlags, "--fast-list")
	}
	if bwlimit != "" {
		p.mountFlags = append(p.mountFlags, bwlimit)
	}

	if loc := op.GetRepository().GetLocation(); loc.GetRemote() == alias && remoteNested(loc.GetPath(), path) {
		return p, invalid("the folder to back up is, holds or is inside the repository")
	}

	args, err := remoteBackupFlags(op)
	if err != nil {
		return p, err
	}
	p.backup = command{repo: op.GetRepository(), verb: "backup", args: append(args, "--", ".")}
	return p, nil
}

// remoteBackupFlags are restic's flags of a backup of a remote: the spec's
// tags, host, exclude_caches, exclude_larger_than and dry run, and always
// --ignore-inode (which also ignores ctime): a mount has no stable inode
// numbers and a mirror downloaded again gets new ones, so unchanged files
// are found by size and modification time, which rclone keeps.
func remoteBackupFlags(op *syncrv1.ResticBackup) ([]string, error) {
	args, err := tagFlags("--tag=", op.GetTags())
	if err != nil {
		return nil, err
	}
	if h := op.GetHost(); h != "" {
		if err := checkText("host", h); err != nil {
			return nil, err
		}
		args = append(args, "--host="+h)
	}
	if op.GetExcludeCaches() {
		args = append(args, "--exclude-caches")
	}
	if op.ExcludeLargerThanBytes != nil {
		n := op.GetExcludeLargerThanBytes()
		if n <= 0 {
			return nil, invalid("exclude_larger_than_bytes must be positive, got %d", n)
		}
		args = append(args, fmt.Sprintf("--exclude-larger-than=%d", n))
	}
	if op.GetDryRun() {
		args = append(args, "--dry-run")
	}
	return append(args, "--ignore-inode"), nil
}

// checkSourcePath rejects a folder path within a remote that has a control
// character or a ".." segment (the controller sends only relative paths
// without one).
func checkSourcePath(path string) error {
	if strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return invalid("the path of the folder to back up contains a control character")
	}
	for seg := range strings.FieldsFuncSeq(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return invalid("the path of the folder to back up must not contain a .. segment")
		}
	}
	return nil
}

// remoteNested reports whether two paths on the same remote are the same
// folder or one is inside the other.
func remoteNested(a, b string) bool {
	a, b = strings.Trim(a, "/"), strings.Trim(b, "/")
	return a == "" || b == "" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// remoteSourceAvailable fails with TOOL_UNAVAILABLE when this worker
// cannot use method, with the reason its Hello gives.
func (e *engine) remoteSourceAvailable(method syncrv1.ResticRemoteSourceMethod) error {
	rs := e.cfg.RemoteSources
	sup := rs.Support
	reason := func(r string) string {
		if r == "" {
			return "this worker does not report it"
		}
		return r
	}
	if method == syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT {
		if rs.Store == nil || rs.Mounter == nil || !sup.GetMount() {
			return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE,
				"this worker cannot mount remotes: %s", reason(sup.GetMountReason()))
		}
		return nil
	}
	if rs.Store == nil || !sup.GetMirror() {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE,
			"this worker does not keep mirrors of remotes: %s", reason(sup.GetMirrorReason()))
	}
	return nil
}

// remoteRun is one backup of a remote.
type remoteRun struct {
	e       *engine
	job     runner.Job
	plan    remotePlan
	emit    runner.EmitFunc
	secrets []string
	// sc scrubs the remotes' secrets, plain and in the obscured form the
	// children are given.
	sc *scrub.Scrubber
	// scratch is the job's private directory: rclone's cache and temp
	// folders, the filters file and restic's temp folder.
	scratch string
	// env defines the remotes for rclone, isolated in scratch.
	env   map[string]string
	stats *syncrv1.RemoteSourceStats
	// empty reports that the check found the folder empty (and
	// allow_empty let the run go on).
	empty bool
}

// runRemoteSource runs a backup of a folder of a remote: check (list the
// folder: NOT_FOUND, SOURCE_EMPTY), read (bring the mirror up to date, or
// mount the folder), back up "." there with restic, clean up (discard the
// mirror's copy if asked, unmount). See ResticBackupRemoteSource in
// link.proto for the contract.
func (e *engine) runRemoteSource(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	if !jobIDPattern.MatchString(job.ID) {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "invalid job ID %q", job.ID)
	}
	if emit == nil {
		emit = func(runner.Progress) {}
	}
	secrets := secretsOf(job.Spec)
	rs := &remoteRun{e: e, job: job, emit: emit, secrets: secrets, sc: scrub.New(secrets)}
	res, err := rs.run(ctx)
	if err != nil && !runner.Canceled(err) && ctx.Err() == nil {
		err = rs.sc.Error(err)
	}
	for i, w := range res.Warnings {
		res.Warnings[i] = rs.sc.String(w)
	}
	res.Warnings = runner.LimitWarnings(res.Warnings)
	return res, err
}

func (rs *remoteRun) run(ctx context.Context) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	plan, err := planRemoteSource(rs.job.Spec)
	if err != nil {
		return res, err
	}
	rs.plan = plan
	method := plan.src.GetMethod()
	if err := rs.e.remoteSourceAvailable(method); err != nil {
		return res, err
	}
	rs.stats = &syncrv1.RemoteSourceStats{Method: method}

	scratch, cleanup, err := rs.e.scratchDir("restic-"+rs.job.ID, rs.job.ID)
	if err != nil {
		return res, err
	}
	defer cleanup()
	rs.scratch = scratch
	if err := rs.prepareEnv(); err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(scratch, filtersFileName), rclonetool.FiltersFile(plan.filters), 0o600); err != nil {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "write the filters file: %w", err)
	}

	// 1. Check, before anything changes on the worker.
	if err := rs.precheck(ctx); err != nil {
		return res, err
	}
	// 2 to 4: read, back up, clean up.
	if method == syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT {
		return rs.viaMount(ctx)
	}
	return rs.viaMirror(ctx)
}

// prepareEnv builds rclone's environment: the remotes (RCLONE_CONFIG_*
// only, as RemoteEnv defines them) and the variables that keep rclone's
// cache and temporary files in the job's scratch directory. rclone also
// gets "--config=", so no rclone.conf is ever read.
func (rs *remoteRun) prepareEnv() error {
	remotes := rs.job.Spec.GetRemotes()
	base, err := rs.e.baseEnv(remotes, "", true)
	if err != nil {
		return err
	}
	env := map[string]string{}
	for k, v := range base {
		if strings.HasPrefix(strings.ToUpper(k), "RCLONE_CONFIG_") {
			env[k] = v
		}
	}
	if obscured := obscuredSecrets(remotes, base); len(obscured) > 0 {
		rs.sc = scrub.New(append(slices.Clip(rs.secrets), obscured...))
	}
	cache, tmp := filepath.Join(rs.scratch, "rclone-cache"), filepath.Join(rs.scratch, "rclone-tmp")
	for _, d := range []string{cache, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "create rclone's folders: %w", err)
		}
	}
	env["RCLONE_CACHE_DIR"] = cache
	env["RCLONE_TEMP_DIR"] = tmp
	env["RCLONE_ASK_PASSWORD"] = "false"
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		env[name] = tmp
	}
	rs.env = env
	return nil
}

// where names the folder in messages: its path on the remote (the alias is
// internal to the job).
func (rs *remoteRun) where() string {
	if p := rs.plan.src.GetPath(); p != "" {
		return fmt.Sprintf("%q on the remote", p)
	}
	return "the remote's root"
}

// phase returns the emit function of a phase: the job's, with Phase set.
func (rs *remoteRun) phase(name string) runner.EmitFunc {
	return func(p runner.Progress) {
		p.Phase = name
		rs.emit(p)
	}
}

// rclone runs the pinned rclone with args and the job's remotes.
func (rs *remoteRun) rclone(ctx context.Context, args []string, stdout, stderr proc.LineFunc) (proc.Result, error) {
	return rs.e.cfg.Supervisor.Run(ctx, proc.Spec{
		Path:   rs.e.cfg.RclonePath,
		Args:   args,
		Dir:    rs.e.cfg.ScratchRoot,
		Env:    maps.Clone(rs.env),
		Stdout: stdout,
		Stderr: stderr,
	})
}

// rcloneOutcome maps the result of an rclone run to the runner's error,
// as the rclone runner maps a transfer's: canceled stays canceled, a binary
// that cannot start is TOOL_UNAVAILABLE, an exit is classified by
// rclonetool (NOT_FOUND, TEMPORARY, EXECUTION_FAILED...).
func (rs *remoteRun) rcloneOutcome(cmd string, err error, pres proc.Result, diag []string) error {
	switch {
	case err == nil:
		return nil
	case runner.Canceled(err):
		return err
	}
	if _, ok := errors.AsType[*proc.StartError](err); ok {
		return runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE, fmt.Errorf("cannot start rclone: %w", err))
	}
	if _, ok := errors.AsType[*proc.ExitError](err); !ok {
		if pres.ExitCode == 0 {
			// rclone succeeded, but a process it started kept the output
			// open past the grace period: its work is complete.
			rs.e.log.Warn("rclone output not fully collected", "job_id", rs.job.ID, "error", err.Error())
			return nil
		}
		return runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, err)
	}
	if rclonetool.ClassifyExit(pres.ExitCode).Success {
		return nil
	}
	return rclonetool.Failure(cmd, pres.ExitCode, diag, err)
}

// precheck lists the folder one level deep, without the filters
// ("rclone lsf --max-depth=1"): a folder that does not exist is NOT_FOUND
// (rclone exit 3; on S3 and B2 that is a bucket that does not exist, while
// a prefix without objects lists as an empty folder), one without entries
// is SOURCE_EMPTY unless allow_empty is set, and any other failure is
// classified as for transfers. A path that names a file lists that file,
// so it counts as a folder that holds it.
func (rs *remoteRun) precheck(ctx context.Context) error {
	entries := 0
	stdout := func(l proc.Line) {
		if len(bytes.TrimSpace(l.Data)) > 0 {
			entries++
		}
	}
	t := rclonetool.NewTracker(nil, rs.sc)
	args := []string{"lsf", "--config=", "--use-json-log", "--log-level=NOTICE", "--max-depth=1", "--", rs.plan.target}
	pres, err := rs.rclone(ctx, args, stdout, t.Line)
	if err := rs.rcloneOutcome("lsf", err, pres, t.Diagnostics()); err != nil {
		code := runner.CodeOf(err)
		switch {
		case runner.Canceled(err):
			return err
		case code == syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND:
			return runner.Errorf(code, "the folder %s does not exist: %w", rs.where(), err)
		}
		return runner.Errorf(code, "cannot list the folder %s: %w", rs.where(), err)
	}
	rs.empty = entries == 0
	if rs.empty && !rs.plan.src.GetAllowEmpty() {
		return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_SOURCE_EMPTY,
			"the folder %s is empty, so nothing was backed up (an emptied or wrong folder would push real snapshots out of retention); "+
				"allow an empty folder in the backup's options to back it up anyway", rs.where())
	}
	return nil
}

// viaMirror brings the mirror up to date, backs it up, and discards its
// copy if the spec asks to, whatever the outcome.
func (rs *remoteRun) viaMirror(ctx context.Context) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	key := rs.plan.src.GetSourceKey()
	m, err := rs.e.cfg.RemoteSources.Store.OpenMirror(key)
	switch {
	case errors.Is(err, sourcestate.ErrBusy):
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, err)
	case err != nil:
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "open the mirror: %w", err)
	}
	defer func() {
		if err := m.Close(); err != nil {
			rs.e.log.Warn("cannot record the run of a mirror or release its lock", "job_id", rs.job.ID, "error", err.Error())
		}
	}()
	discard := func() {
		if !rs.plan.src.GetDiscardMirror() || rs.stats.GetMirrorDiscarded() {
			return
		}
		if err := proc.RemoveAll(m.DataDir()); err != nil {
			rs.e.log.Warn("cannot delete the copy of a mirror the backup asked to discard", "job_id", rs.job.ID, "error", err.Error())
			return
		}
		rs.stats.MirrorDiscarded = true
	}
	defer discard()

	res, err = rs.sync(ctx, m.DataDir())
	if err != nil {
		return res, err
	}
	res, err = rs.backup(ctx, m.DataDir())
	discard()
	return res, err
}

// sync brings the mirror's copy at data up to date (phase "mirror"):
// rclone sync with the filters, deleting what the folder no longer holds or
// the filters now leave out (--delete-excluded), empty folders included.
// rclone keeps the modification times, so restic finds unchanged files
// unchanged. A failure fails the job; the copy stays as it is (rclone
// deletes nothing after an error).
func (rs *remoteRun) sync(ctx context.Context, data string) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	args := []string{"sync", "--config="}
	args = append(args, rclonetool.BaseFlags()...)
	args = append(args, "--create-empty-src-dirs", "--delete-excluded", "--filter-from="+filepath.Join(rs.scratch, filtersFileName))
	args = append(args, rs.plan.syncFlags...)
	args = append(args, "--", rs.plan.target, data)

	t := rclonetool.NewTracker(rs.phase(link.ProgressPhaseMirror), rs.sc)
	start := time.Now()
	pres, err := rs.rclone(ctx, args, t.Line, t.Line)
	p := t.Progress()
	p.Phase = link.ProgressPhaseMirror
	p.Elapsed = max(p.Elapsed, pres.Duration)
	res.ExitCode, res.Stats = pres.ExitCode, p
	rs.stats.MirroredBytes, rs.stats.MirroredFiles, rs.stats.DeletedFiles = p.Bytes, p.Files, p.Deletes
	rs.stats.MirrorElapsed = durationpb.New(time.Since(start))
	if err := rs.rcloneOutcome("sync", err, pres, t.Diagnostics()); err != nil {
		if runner.Canceled(err) {
			return res, err
		}
		if diskFull(t.Diagnostics()) {
			return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
				"the worker's disk is full: the mirror of the folder does not fit in %s; what was downloaded is kept for the next run. "+
					"Free space on the worker, leave files out with filters, or use the mount method: %w",
				rs.e.cfg.RemoteSources.Store.MirrorsDir(), err)
		}
		return res, runner.Errorf(runner.CodeOf(err), "the mirror of the folder %s could not be brought up to date, so nothing was backed up: %w", rs.where(), err)
	}
	// An empty folder (allow_empty) may leave nothing behind to back up.
	if err := os.MkdirAll(data, 0o700); err != nil {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "create the mirror's folder: %w", err)
	}
	return res, nil
}

// diskFull reports whether rclone's messages say that the disk is full
// (ENOSPC on Unix, ERROR_DISK_FULL on Windows).
func diskFull(diag []string) bool {
	for _, d := range diag {
		l := strings.ToLower(d)
		if strings.Contains(l, "no space left on device") || strings.Contains(l, "not enough space on the disk") {
			return true
		}
	}
	return false
}

// viaMount mounts the folder, backs the mount up and unmounts it, on every
// path out.
func (rs *remoteRun) viaMount(ctx context.Context) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	key := rs.plan.src.GetSourceKey()
	src := rs.e.cfg.RemoteSources
	lock, err := src.Store.LockMount(key)
	switch {
	case errors.Is(err, sourcestate.ErrBusy):
		return res, runner.Fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, err)
	case err != nil:
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "lock the mount point: %w", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			rs.e.log.Warn("cannot release the lock of a mount point", "job_id", rs.job.ID, "error", err.Error())
		}
	}()
	mt, err := src.Mounter.Mount(ctx, mount.Request{
		Remote:  rs.plan.target,
		Point:   lock.Point(),
		DevName: "syncr-" + key,
		Flags:   append([]string{"--filter-from=" + filepath.Join(rs.scratch, filtersFileName)}, rs.plan.mountFlags...),
		Env:     maps.Clone(rs.env),
		Dir:     rs.e.cfg.ScratchRoot,
		Timeout: src.MountTimeout,
		Scrub:   rs.sc,
	})
	if err != nil {
		if ctx.Err() != nil {
			return res, fmt.Errorf("mount the folder: %w", err)
		}
		if me, ok := errors.AsType[*mount.Error](err); ok {
			code := syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
			if me.Unavailable {
				code = syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE
			}
			return res, runner.Errorf(code, "cannot mount the folder %s: %w", rs.where(), err)
		}
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "mount the folder: %w", err)
	}
	unmounted := false
	unmount := func() error {
		if unmounted {
			return nil
		}
		unmounted = true
		err := mt.Unmount()
		if err != nil {
			rs.e.log.Warn("cannot unmount a remote after its backup; the next run and the worker's next start try again",
				"job_id", rs.job.ID, "mount_point", lock.Point(), "error", rs.sc.String(err.Error()))
		}
		return err
	}
	defer func() { _ = unmount() }()

	res, err = rs.backup(ctx, lock.Point())
	if uerr := unmount(); uerr != nil && err == nil {
		res.Warnings = append(res.Warnings, "the remote could not be unmounted after the backup; the next run and the worker's next start try again: "+uerr.Error())
	}
	return res, err
}

// backup runs restic in dir, the mirror's copy or the mount point, and
// backs up "." (phase "backup"), and adds the remote source's stats to the
// output.
func (rs *remoteRun) backup(ctx context.Context, dir string) (runner.Result, error) {
	res, s, sc, err := rs.e.exec(ctx, execution{
		kind: kindBackup, dir: rs.scratch, workdir: dir, logID: rs.job.ID,
		remotes: rs.job.Spec.GetRemotes(), cmd: rs.plan.backup, emit: rs.phase(link.ProgressPhaseBackup), secrets: rs.secrets,
	})
	res.Stats.Phase = link.ProgressPhaseBackup
	if ee, ok := errors.AsType[*ExitError](err); ok && ee.ExitCode == exitFatal && strings.Contains(ee.Message, emptySnapshot) {
		// restic makes no snapshot of a folder without any entry.
		if !rs.empty {
			return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED,
				"nothing to back up: the filters leave out everything in the folder %s, and restic makes no empty snapshot", rs.where())
		}
		res.Output = &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{RemoteSource: rs.stats}}}
		res.Warnings = []string{fmt.Sprintf("the folder %s is empty and restic makes no empty snapshot, so no snapshot was made", rs.where())}
		return res, nil
	}
	if err == nil && res.Output == nil {
		var out *syncrv1.JobOutput
		if out, err = s.output(rs.job.Spec); err != nil {
			err = runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "restic backup: %w", err)
		}
		res.Output = out
	}
	if b := res.Output.GetResticBackup(); b != nil {
		b.RemoteSource = rs.stats
	}
	if err != nil && !runner.Canceled(err) {
		err = sc.Error(err)
	}
	for i, w := range res.Warnings {
		res.Warnings[i] = sc.String(w)
	}
	return res, err
}
