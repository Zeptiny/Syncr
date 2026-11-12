package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

const kindDeleteData = "restic_delete_data"

// repositoryParts are the directories of restic's repository layout in the
// order they are deleted: snapshots and index go before data, so an
// interrupted deletion leaves a repository with no snapshots rather than
// snapshots pointing at missing packs; keys go last, so a retry can still
// verify the repository until the very end. The config file follows them.
func repositoryParts() []string {
	return []string{"locks", "snapshots", "index", "data", "keys"}
}

const (
	configPart = "config"
	// dataPart holds its files in sub-folders.
	dataPart = "data"
)

// restic's file names, as rclone filter rules relative to the folder they
// apply to. A {{ }} segment is a regular expression matched,
// case-sensitively, against one whole path segment.
const (
	// fileRule: a file named by 64 lower-case hex digits, as in keys/,
	// locks/, snapshots/ and index/.
	fileRule = "/{{[0-9a-f]{64}}}"
	// packRule: such a file in a sub-folder named by 2 hex digits, as in
	// data/.
	packRule = "/{{[0-9a-f]{2}}}/{{[0-9a-f]{64}}}"
	// packDirContentRule: whatever lies directly in a sub-folder of data/
	// named by 2 hex digits. rclone rmdirs sees nothing else, so it removes
	// only those sub-folders, and only the ones that hold no file;
	// one that holds only a folder looks empty to it, and the backend
	// refuses to remove it.
	packDirContentRule = "/{{[0-9a-f]{2}}}/*"
)

// partRule returns the filter rule of restic's files in part (relative to
// the part's folder) and the depth of those files below it.
func partRule(part string) (rule string, depth int) {
	if part == dataPart {
		return packRule, 2
	}
	return fileRule, 1
}

// layoutRules are the filter flags of every file of restic's layout,
// relative to the repository's location (three levels deep at most).
func layoutRules() []string {
	return []string{
		"--include=/" + configPart,
		"--include=/{{locks|snapshots|index|keys}}" + fileRule,
		"--include=/" + dataPart + packRule,
	}
}

// isPackDir reports whether name is the name of one of data/'s
// sub-folders: two lower-case hex digits.
func isPackDir(name string) bool {
	return len(name) == 2 && isLowerHex(name[0]) && isLowerHex(name[1])
}

func isLowerHex(c byte) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' }

// maxKeptNames is how many of the entries kept inside restic's folders the
// warning names.
const maxKeptNames = 8

// deleteRunner runs restic_delete_data: it deletes restic's own files at a
// repository's location, after verifying that the location still holds the
// expected repository. Only files named like restic's, in restic's
// folders, are deleted, and only folders left empty are removed: other
// files there are never touched (also another repository stored inside
// one of those folders), and the ones inside restic's folders are named in
// a warning. The location itself is removed only when asked to and empty.
//
// restic has no "delete repository" command, so the files are deleted with
// the pinned rclone, started directly with the job's remotes in its
// environment and the flags restic's own rclone gets (no rclone.conf, real
// deletes on B2 and Google Drive).
type deleteRunner struct{ e *engine }

// Kind implements runner.Runner.
func (r *deleteRunner) Kind() string { return kindDeleteData }

// Run implements runner.Runner.
func (r *deleteRunner) Run(ctx context.Context, job runner.Job, emit runner.EmitFunc) (runner.Result, error) {
	res := runner.Result{ExitCode: -1}
	if job.Spec == nil {
		return res, invalid("job has no spec")
	}
	if !jobIDPattern.MatchString(job.ID) {
		return res, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "invalid job ID %q", job.ID)
	}
	secrets := secretsOf(job.Spec)
	d := &deletion{e: r.e, job: job, op: job.Spec.GetResticDeleteData(), emit: emit, start: time.Now(), sc: scrub.New(secrets), secrets: secrets}
	out, err := d.run(ctx)
	res.Stats = runner.Progress{Deletes: d.deleted, Elapsed: time.Since(d.start)}
	if err != nil {
		if !runner.Canceled(err) {
			err = d.sc.Error(err)
		}
		return res, err
	}
	res.ExitCode = exitOK
	res.Output = &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticDeleteData{ResticDeleteData: out}}
	if w := d.keptWarning(); w != "" {
		res.Warnings = runner.LimitWarnings([]string{d.sc.String(w)})
	}
	return res, nil
}

// deletion is the state of one restic_delete_data run.
type deletion struct {
	e       *engine
	job     runner.Job
	op      *syncrv1.ResticDeleteData
	emit    runner.EmitFunc
	start   time.Time
	sc      *scrub.Scrubber
	secrets []string

	scratch string
	// loc is the repository's location as an rclone path, and local whether
	// it is a path on the worker's filesystem.
	loc   string
	local bool
	env   map[string]string

	deleted int64 // objects deleted so far
	removed []string
	// kept are the entries inside restic's folders that are not restic's,
	// relative to the location ("data/notes.txt", "keys/old/").
	kept []string
}

func (d *deletion) run(ctx context.Context) (*syncrv1.ResticDeleteDataOutput, error) {
	if d.op == nil {
		return nil, noOperation(kindDeleteData)
	}
	if !resticIDPattern.MatchString(d.op.GetExpectedResticId()) {
		return nil, invalid("expected_restic_id must be 64 hexadecimal digits")
	}
	if err := d.prepare(); err != nil {
		return nil, err
	}
	scratch, cleanup, err := d.e.scratchDir("restic-"+d.job.ID, d.job.ID)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	d.scratch = scratch
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		d.env[name] = scratch
	}

	// 1. Verify that the location still holds the expected repository.
	cfg, verr := d.catConfig(ctx)
	switch {
	case verr == nil:
		if cfg.GetId() != d.op.GetExpectedResticId() {
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_MISMATCH,
				"the location holds another repository (%.8s, expected %.8s); nothing was deleted", cfg.GetId(), d.op.GetExpectedResticId())
		}
	case runner.Canceled(verr):
		return nil, verr
	case errors.Is(verr, ErrWrongPassword):
		// An earlier attempt may have stopped between keys/ and config:
		// then no key is left and only the config remains to delete. A
		// first attempt cannot tell that from a config it may not delete.
		wrong := runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD,
			"the stored password does not open the repository; nothing was deleted")
		if !d.op.GetResume() {
			return nil, wrong
		}
		top, err := d.ls(ctx, d.loc, "--max-depth=1")
		if err != nil {
			return nil, err
		}
		keys, err := d.ls(ctx, d.part("keys"), "--max-depth=1", "--files-only", "--include="+fileRule)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(top.paths(), configPart) || len(keys.entries) > 0 {
			return nil, wrong
		}
		return d.finish(ctx, nil)
	case errors.Is(verr, ErrRepositoryNotFound):
		files, err := d.ls(ctx, d.loc, append([]string{"-R", "--files-only", "--max-depth=3"}, layoutRules()...)...)
		if err != nil {
			return nil, err
		}
		if left := remaining(files.paths()); len(left) > 0 {
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND,
				"restic's config is missing but other repository files remain (%s); nothing was deleted", strings.Join(left, ", "))
		}
		if !d.op.GetResume() {
			// Nothing was ever deleted here: the repository is elsewhere
			// (another worker's disk, or changed remote settings).
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND,
				"no restic repository was found at the location on this worker, so nothing was deleted; check the worker and the remote's settings, or delete the repository without deleting its data")
		}
		return &syncrv1.ResticDeleteDataOutput{AlreadyGone: true}, nil
	default:
		return nil, verr
	}

	// 2. No other restic client may be using the repository.
	if err := d.checkLocks(ctx); err != nil {
		return nil, err
	}

	// 3. Delete the parts in order.
	top, err := d.ls(ctx, d.loc, "--max-depth=1")
	if err != nil {
		return nil, err
	}
	return d.finish(ctx, top.paths())
}

// finish deletes the parts that exist (names; nil deletes none), then the
// config, then the directory if asked to.
func (d *deletion) finish(ctx context.Context, names []string) (*syncrv1.ResticDeleteDataOutput, error) {
	for _, part := range repositoryParts() {
		if !slices.Contains(names, part) {
			continue
		}
		if err := d.deletePart(ctx, part); err != nil {
			return nil, d.partial(part, err)
		}
		d.removed = append(d.removed, part)
	}
	// 4. The config is the commit point.
	if _, _, err := d.rclone(ctx, nil, "deletefile", "--", d.part(configPart)); err != nil {
		return nil, d.partial(configPart, err)
	}
	d.deleted++
	d.removed = append(d.removed, configPart)
	d.publish()

	out := &syncrv1.ResticDeleteDataOutput{DeletedObjects: d.deleted, RemovedParts: d.removed}
	// 5. Remove the directory when it is empty; never the remote's root.
	if d.op.GetRemoveDirectory() && d.op.GetRepository().GetLocation().GetPath() != "" {
		rest, err := d.ls(ctx, d.loc, "--max-depth=1")
		switch {
		case err != nil && runner.Canceled(err):
			return nil, err
		case err != nil:
			// The data is gone; an unreadable directory is only left behind.
			d.e.log.Warn("cannot list the repository directory after the deletion", "job_id", d.job.ID, "error", d.sc.String(err.Error()))
		case len(rest.entries) == 0:
			out.DirectoryRemoved = true
			if rest.exists {
				_, _, rmErr := d.rclone(ctx, nil, "rmdir", "--", d.loc)
				if rmErr != nil && runner.Canceled(rmErr) {
					return nil, rmErr
				}
				out.DirectoryRemoved = rmErr == nil
			}
		}
	}
	return out, nil
}

// partial describes a failure part-way: what is already gone.
func (d *deletion) partial(part string, err error) error {
	if runner.Canceled(err) {
		return err
	}
	done := "nothing was deleted yet"
	if len(d.removed) > 0 {
		done = "deleted " + strings.Join(d.removed, ", ")
	}
	return runner.Errorf(runner.CodeOf(err), "%s; failed while deleting %s: %w", done, part, err)
}

// remaining returns the parts of restic's layout (config and the folders)
// that paths, relative to the location, lie in, in layout order.
func remaining(paths []string) []string {
	var left []string
	for _, part := range append([]string{configPart}, repositoryParts()...) {
		if slices.ContainsFunc(paths, func(p string) bool { return p == part || strings.HasPrefix(p, part+"/") }) {
			left = append(left, part)
		}
	}
	return left
}

// prepare resolves the location and the environment of the rclone runs.
func (d *deletion) prepare() error {
	repo := d.op.GetRepository()
	value, viaRclone, err := d.e.repository(d.job.Spec.GetRemotes(), repo)
	if err != nil {
		return err
	}
	d.env = map[string]string{}
	if !viaRclone {
		d.loc, d.local = value, true
		return nil
	}
	d.loc = strings.TrimPrefix(value, "rclone:")
	env, err := d.e.baseEnv(d.job.Spec.GetRemotes(), "", true)
	if err != nil {
		return err
	}
	for k, v := range env {
		if strings.HasPrefix(strings.ToUpper(k), "RCLONE_CONFIG_") {
			d.env[k] = v
		}
	}
	if obscured := obscuredSecrets(d.job.Spec.GetRemotes(), env); len(obscured) > 0 {
		d.sc = scrub.New(append(slices.Clip(d.secrets), obscured...))
	}
	return nil
}

// part returns the rclone path of one entry of the repository.
func (d *deletion) part(name string) string {
	switch {
	case d.local:
		return filepath.Join(d.loc, name)
	case strings.HasSuffix(d.loc, ":"):
		return d.loc + name
	}
	return strings.TrimSuffix(d.loc, "/") + "/" + name
}

// restic runs one restic command of the deletion.
func (d *deletion) restic(ctx context.Context, cmd command) (*session, error) {
	_, s, _, err := d.e.exec(ctx, execution{
		kind: kindDeleteData, dir: d.scratch, logID: d.job.ID,
		remotes: d.job.Spec.GetRemotes(), cmd: cmd, secrets: d.secrets,
	})
	return s, err
}

func (d *deletion) catConfig(ctx context.Context) (*syncrv1.ResticRepositoryConfig, error) {
	s, err := d.restic(ctx, catConfig(d.op.GetRepository()))
	if err != nil {
		return nil, err
	}
	cfg, err := s.repositoryConfig()
	if err != nil {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "cannot verify the repository; nothing was deleted: %w", err)
	}
	return cfg, nil
}

// checkLocks removes stale locks and fails if any lock remains.
func (d *deletion) checkLocks(ctx context.Context) error {
	repo := d.op.GetRepository()
	if _, err := d.restic(ctx, command{repo: repo, verb: "unlock", noCache: true}); err != nil {
		if runner.Canceled(err) {
			return err
		}
		return runner.Errorf(runner.CodeOf(err), "cannot check the repository's locks; nothing was deleted: %w", err)
	}
	s, err := d.restic(ctx, command{repo: repo, verb: "list", global: []string{"--no-lock"}, args: []string{"locks"}, noCache: true, collect: true})
	if err != nil {
		if runner.Canceled(err) {
			return err
		}
		return runner.Errorf(runner.CodeOf(err), "cannot check the repository's locks; nothing was deleted: %w", err)
	}
	for line := range bytes.SplitSeq(s.raw, []byte("\n")) {
		if resticIDPattern.Match(bytes.TrimSpace(line)) {
			return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED,
				"another restic client is using the repository; nothing was deleted")
		}
	}
	return nil
}

// deletePart deletes restic's files in one folder of the repository
// (partRule), then the sub-folders of data/ and the folder itself if they
// are empty. Whatever else the folder holds stays and is noted in d.kept.
func (d *deletion) deletePart(ctx context.Context, name string) error {
	base := d.deleted
	onLine := func(l proc.Line) {
		var m struct {
			Stats *struct {
				Deletes int64 `json:"deletes"`
			} `json:"stats"`
		}
		if json.Unmarshal(bytes.TrimSpace(l.Data), &m) == nil && m.Stats != nil && base+m.Stats.Deletes > d.deleted {
			d.deleted = base + m.Stats.Deletes
			d.publish()
		}
	}
	dir := d.part(name)
	rule, depth := partRule(name)
	_, detail, err := d.rclone(ctx, onLine, "delete", "--include="+rule, fmt.Sprintf("--max-depth=%d", depth),
		"--use-json-log", "--stats=1s", "--stats-log-level=NOTICE", "--", dir)
	if err != nil && !isNotFound(detail) {
		return err
	}
	if name == dataPart {
		// The emptied sub-folders (object stores have none). One that still
		// holds something stays, and the backend's refusal is no error.
		_, _, err := d.rclone(ctx, nil, "rmdirs", "--leave-root", "--include="+packDirContentRule, "--max-depth=2", "--retries=1", "--", dir)
		if err != nil && runner.Canceled(err) {
			return err
		}
	}
	rest, err := d.ls(ctx, dir, "-R", fmt.Sprintf("--max-depth=%d", depth))
	if err != nil {
		return err
	}
	kept := foreign(name, rest)
	d.kept = append(d.kept, kept...)
	if len(kept) > 0 {
		d.e.log.Warn("kept entries in a repository folder that restic did not write", "job_id", d.job.ID, "part", name,
			"count", len(kept), "entries", d.sc.String(strings.Join(kept[:min(len(kept), maxKeptNames)], ", ")))
	}
	if rest.exists && len(rest.entries) == 0 {
		if _, _, err := d.rclone(ctx, nil, "rmdir", "--", dir); err != nil && runner.Canceled(err) {
			return err
		}
	}
	return nil
}

// foreign returns what a part's listing after its deletion holds that is
// not restic's, relative to the location (folders with a trailing "/").
// In data/ the sub-folders named by 2 hex digits are restic's: their
// entries are named instead (an empty one is nobody's concern).
func foreign(part string, l listing) []string {
	packDirs := map[string]bool{}
	if part == dataPart {
		for _, e := range l.entries {
			if e.dir && isPackDir(e.path) {
				packDirs[e.path] = true
			}
		}
	}
	var out []string
	for _, e := range l.entries {
		parent, _, nested := strings.Cut(e.path, "/")
		if packDirs[e.path] || nested && !packDirs[parent] {
			continue
		}
		name := part + "/" + e.path
		if e.dir {
			name += "/"
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// keptWarning describes the entries kept inside restic's folders, or "".
func (d *deletion) keptWarning() string {
	if len(d.kept) == 0 {
		return ""
	}
	names := strings.Join(d.kept[:min(len(d.kept), maxKeptNames)], ", ")
	if more := len(d.kept) - maxKeptNames; more > 0 {
		names += fmt.Sprintf(" and %d more", more)
	}
	noun := "entries"
	if len(d.kept) == 1 {
		noun = "entry"
	}
	return fmt.Sprintf("kept %d %s inside restic's folders that restic did not write: %s", len(d.kept), noun, names)
}

func (d *deletion) publish() {
	d.emit(runner.Progress{Deletes: d.deleted, Elapsed: time.Since(d.start)})
}

// listing is the outcome of listing a directory.
type listing struct {
	entries []entry
	exists  bool
}

// entry is one listed entry: its path relative to the listed directory
// ("/" separated) and whether it is a directory.
type entry struct {
	path string
	dir  bool
}

// paths returns the paths of l's entries.
func (l listing) paths() []string {
	out := make([]string, len(l.entries))
	for i, e := range l.entries {
		out[i] = e.path
	}
	return out
}

// ls lists dir with rclone lsjson and the flags (depth, filters); a
// directory that does not exist has no entries.
func (d *deletion) ls(ctx context.Context, dir string, flags ...string) (listing, error) {
	var out listing
	onLine := func(l proc.Line) {
		data := bytes.TrimSuffix(bytes.TrimSpace(l.Data), []byte(","))
		if len(data) == 0 || data[0] != '{' {
			return
		}
		var e struct {
			Path  string `json:"Path"`
			IsDir bool   `json:"IsDir"`
		}
		if json.Unmarshal(data, &e) == nil && e.Path != "" {
			out.entries = append(out.entries, entry{path: e.Path, dir: e.IsDir})
		}
	}
	args := append([]string{"lsjson", "--no-mimetype", "--no-modtime"}, flags...)
	_, detail, err := d.rclone(ctx, onLine, append(args, "--", dir)...)
	if err != nil {
		if isNotFound(detail) {
			return listing{}, nil
		}
		return listing{}, runner.Errorf(runner.CodeOf(err), "cannot list the repository's location: %w", err)
	}
	out.exists = true
	return out, nil
}

// isNotFound reports whether rclone's diagnostics say the directory does
// not exist.
func isNotFound(detail string) bool {
	return strings.Contains(detail, "directory not found")
}

// deleteFlags are the global rclone flags of the deletion: no rclone.conf
// of the worker's user, and real deletes on B2 and Google Drive (restic's
// own rclone gets the same, see rcloneArgs).
func deleteFlags() []string {
	return []string{"--config=", "--b2-hard-delete", "--drive-use-trash=false"}
}

// rclone runs the pinned rclone with args. Standard output lines go to
// onStdout; standard error is kept as scrubbed diagnostics (JSON log lines
// go to onStdout as well, so stats are seen wherever rclone prints them).
// It returns the exit code and the diagnostics, also on failure.
func (d *deletion) rclone(ctx context.Context, onStdout func(proc.Line), args ...string) (int, string, error) {
	var tail []string
	keep := func(l proc.Line) {
		data := bytes.TrimSpace(l.Data)
		if len(data) == 0 {
			return
		}
		if data[0] == '{' {
			var m struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
			}
			if json.Unmarshal(data, &m) == nil {
				if onStdout != nil {
					onStdout(l)
				}
				if m.Level != "error" && m.Level != "critical" {
					return
				}
				data = []byte(m.Msg)
			}
		}
		if len(tail) == maxTailLines {
			tail = append(tail[:0], tail[1:]...)
		}
		tail = append(tail, d.sc.Clip(string(data), maxTailLineBytes))
	}
	stdout := func(l proc.Line) {
		if onStdout != nil {
			onStdout(l)
		}
	}
	res, err := d.e.cfg.Supervisor.Run(ctx, proc.Spec{
		Path:   d.e.cfg.RclonePath,
		Args:   append(deleteFlags(), args...),
		Dir:    d.e.cfg.ScratchRoot,
		Env:    d.env,
		Stdout: stdout,
		Stderr: keep,
	})
	detail := joinLines(tail)
	if err == nil || runner.Canceled(err) {
		return res.ExitCode, detail, err
	}
	if _, ok := errors.AsType[*proc.ExitError](err); ok {
		msg := fmt.Sprintf("rclone %s failed (exit code %d)", args[0], res.ExitCode)
		if detail != "" {
			msg += ": " + detail
		}
		return res.ExitCode, detail, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "%s", msg)
	}
	return res.ExitCode, detail, err
}
