package rclone

import (
	"context"
	pathpkg "path"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

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
// A source at the crypt remote's root needs no check: its own listing
// fails with the wrong password unless names are not encrypted, and then
// every file fails to decrypt. Other remotes are not checked: a missing
// prefix of a bucket is an empty folder, as rclone says.
func (r *Runner) checkCryptSource(ctx context.Context, dir string, env *rcloneenv.Env, sc *scrub.Scrubber, loc *syncrv1.Location) error {
	alias := loc.GetRemote()
	if backend, _ := env.BackendType(alias); backend != "crypt" {
		return nil
	}
	path := strings.TrimPrefix(pathpkg.Clean("/"+loc.GetPath()), "/")
	if path == "" {
		return nil
	}
	parent, name := pathpkg.Split(path)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	found := false
	onLine := func(l proc.Line) {
		if e, ok := parseLsjsonLine(l.Data, parent); ok && !found && e.GetName() == name {
			found = true
			stop() // nothing else is needed from the listing
		}
	}
	t := newTracker(func(runner.Progress) {}, sc)
	pres, err := r.cfg.Supervisor.Run(ctx, proc.Spec{
		Path: r.cfg.Binary,
		Args: []string{"lsjson", "--max-depth=1", "--no-mimetype", "--no-modtime", "--", rcloneenv.RemotePath(alias, parent)},
		Dir:  dir, Env: env.Vars(), Stdout: onLine, Stderr: probeStderr(t),
	})
	if found {
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
