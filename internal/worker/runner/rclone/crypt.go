package rclone

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"sync/atomic"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclonetool"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// checkCryptPasswords checks the password of each crypt remote among the
// locations (rclonetool.CryptCheckArgs), once per remote, before a job
// reads or writes them: a wrong password fails the job (EXECUTION_FAILED)
// before rclone touches anything. roles names each location in messages
// ("source", "path2"...).
func (r *Runner) checkCryptPasswords(ctx context.Context, dir string, env *rcloneenv.Env, sc *scrub.Scrubber, roles []string, locs []*syncrv1.Location) error {
	checked := map[string]bool{}
	for i, loc := range locs {
		alias := loc.GetRemote()
		if !isCrypt(env, alias) || checked[alias] {
			continue
		}
		checked[alias] = true
		if err := r.checkCryptPassword(ctx, dir, env, sc, roles[i], alias); err != nil {
			return err
		}
	}
	return nil
}

// isCrypt reports whether alias is a crypt remote of env.
func isCrypt(env *rcloneenv.Env, alias string) bool {
	backend, _ := env.BackendType(alias)
	return backend == "crypt"
}

// checkCryptPassword reads the first block of a file of the crypt remote
// alias (rclonetool.CryptCheck): it fails (EXECUTION_FAILED) when nothing
// decrypts, so the password is wrong, and passes when a block decrypts or
// the remote holds no file with data. A check that cannot run is
// classified as rclone's other failures are. role names the remote in
// messages ("source"; empty for a probe, which names nothing).
//
// Every file-name mode needs it: with obfuscated names a wrong password
// lists garbled names without an error, and on a bucket (S3, B2) a garbled
// folder lists as empty, so a sync from the remote's root would empty its
// destination, and a sync into it would delete the files at its root.
func (r *Runner) checkCryptPassword(ctx context.Context, dir string, env *rcloneenv.Env, sc *scrub.Scrubber, role, alias string) error {
	cctx, stop := context.WithCancel(ctx)
	defer stop()
	t := newTracker(nil, sc)
	check := rclonetool.NewCryptCheck(t.core, stop)
	pres, err := r.cfg.Supervisor.Run(cctx, proc.Spec{
		Path: r.cfg.Binary, Args: rclonetool.CryptCheckArgs(alias), Dir: dir, Env: rclonetool.CryptCheckEnv(env.Vars(), alias),
		Stdout: check.Stdout, StdoutChunks: true, Stderr: check.Stderr,
	})
	if ctx.Err() != nil {
		if _, ok := errors.AsType[*proc.CanceledError](err); ok {
			return err
		}
		// The check ended before the cancel: nothing else started.
		return fmt.Errorf("rclone: canceled after the check of the encrypted remote's password: %w", context.Cause(ctx))
	}
	switch check.Verdict(pres.ExitCode, err) {
	case rclonetool.CryptReadable, rclonetool.CryptNothing:
		return nil
	case rclonetool.CryptUnreadable:
		return rclonetool.CryptUnreadableError(role, t.diagnostics())
	case rclonetool.CryptFailed:
	}
	if oerr := r.outcome(err, pres, t, "cat (a check of an encrypted remote's password)"); oerr != nil {
		if role != "" {
			role += ": "
		}
		return runner.Errorf(runner.CodeOf(oerr), "%scannot check the encrypted remote's password: %w", role, oerr)
	}
	return nil
}

// checkCryptSource refuses (NOT_FOUND) a transfer source on a crypt remote,
// below the crypt remote's root, that its parent folder does not list. It
// lists only the parent, and stops at the folder.
//
// The worker sets strict_names, so a listing with the wrong password fails
// on the names it cannot decrypt, but only where there are names to list.
// With the wrong password a folder's name encrypts to another one, which
// does not exist: a folder-based remote (SFTP) then reports the folder
// missing, but a bucket-based one (S3, B2) lists a prefix without
// objects as an empty folder, without an error. A sync from it would empty
// its destination. The parent's listing either fails (with encrypted
// names, on undecryptable names) or lacks the folder (with obfuscated
// names, or a folder that really does not exist).
//
// It runs after the password check (checkCryptPassword), which finds a
// wrong password whenever the remote holds a file with data. This check
// still makes a folder that is missing NOT_FOUND instead of an empty
// source, also when the password is wrong and the remote holds no such
// file. The crypt remote's root has no parent to look in; other remotes
// are not checked: a missing prefix of a bucket is an empty folder, as
// rclone says.
func (r *Runner) checkCryptSource(ctx context.Context, dir string, env *rcloneenv.Env, sc *scrub.Scrubber, loc *syncrv1.Location) error {
	alias := loc.GetRemote()
	if !isCrypt(env, alias) {
		return nil
	}
	path := strings.TrimPrefix(pathpkg.Clean("/"+loc.GetPath()), "/")
	if path == "" {
		return nil
	}
	parent, name := pathpkg.Split(path)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// The supervisor delivers lines on this goroutine before Run returns;
	// atomic all the same, so the check never depends on it.
	var found atomic.Bool
	onLine := func(l proc.Line) {
		if e, ok := parseLsjsonLine(l.Data, parent); ok && !found.Load() && e.GetName() == name {
			found.Store(true)
			stop() // nothing else is needed from the listing
		}
	}
	t := newTracker(func(runner.Progress) {}, sc)
	pres, err := r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path: r.cfg.Binary,
		Args: []string{"lsjson", "--max-depth=1", "--no-mimetype", "--no-modtime", "--", rcloneenv.RemotePath(alias, parent)},
		Dir:  dir, Env: env.Vars(), Stdout: onLine, Stderr: probeStderr(t),
	})
	if found.Load() {
		return nil
	}
	if err != nil {
		if oerr := r.outcome(err, pres, t, "lsjson (the source's parent folder on a crypt remote)"); oerr != nil {
			return oerr
		}
	}
	return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
		"source: %q is not in its parent folder on the crypt remote %q; if it exists, the remote's password is wrong "+
			"(with another password a folder's name encrypts to another one)", path, alias)
}
