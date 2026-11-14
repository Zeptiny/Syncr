// Package mount mounts a folder of a remote read-only with "rclone mount"
// for the length of one backup (restic_backup with a remote source and the
// mount method), sweeps the mounts a killed worker left behind, and finds
// out at startup whether this machine can mount at all (Detect).
//
// # Lifecycle
//
// Mounter.Mount first sweeps the mount point (a stale mount there, left by
// a killed run, is unmounted), prepares it, then starts rclone mount as a
// supervised child (internal/worker/proc: process tree, the job's cancel,
// time limit and the worker's shutdown) with --read-only and
// --vfs-cache-mode off, and waits until the folder is mounted: on Linux
// until it is listed in /proc/self/mountinfo, on Windows until it exists
// as a folder. It polls every Config.Poll, up to Request.Timeout, and fails
// early when rclone exits first (Error, Unavailable when rclone's messages
// name FUSE or WinFsp).
//
// Mount.Unmount stops rclone, which unmounts the folder itself: on Unix
// the supervisor sends it SIGINT (rclone's documentation: a foreground
// mount is stopped by SIGINT or SIGTERM) and kills it after Config.Grace;
// on Windows the supervisor ends the process, and WinFsp removes a mount
// whose process exited. On Linux a folder still mounted after rclone ended
// (it was killed) is unmounted lazily with "fusermount3 -uz" (or
// "fusermount"). The runner calls Unmount on every path out of a job:
// success, failure, cancel, time limit and shutdown.
//
// # Where the mount point lives
//
// The mount points are <data_dir>/sources/mount/<source_key>
// (internal/worker/sourcestate). On Linux the folder must exist and be
// empty; on Windows it must not exist (rclone's documentation: "the parent
// directory or drive must exist, and mount must not exist"), so Mount
// removes an empty leftover folder there. Nothing in this package stats a
// path below a mount point before it knows the point is not mounted: a
// mount whose rclone process hangs blocks whoever touches it.
//
// Allowed dependencies: the standard library, golang.org/x/sys,
// internal/worker/proc, internal/worker/runner (ValidText),
// internal/worker/runner/rclonetool and internal/worker/runner/scrub.
package mount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclonetool"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// Defaults of Config.
const (
	// DefaultGrace is how long rclone may take to unmount and exit once it
	// is asked to, before it is killed.
	DefaultGrace = 10 * time.Second
	// DefaultPoll is how often Mount checks whether the folder is mounted.
	DefaultPoll = 100 * time.Millisecond
	// helperTimeout bounds one run of the FUSE helper (a lazy unmount).
	helperTimeout = 30 * time.Second
)

// Starter runs a child process and blocks until it has exited;
// *proc.Supervisor implements it.
type Starter interface {
	Run(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

// Clock makes the timers of the readiness wait; tests replace it.
type Clock interface {
	// Ticker returns a channel that receives every d, and its stop
	// function.
	Ticker(d time.Duration) (<-chan time.Time, func())
	// After returns a channel that receives once d has passed, and its
	// stop function.
	After(d time.Duration) (<-chan time.Time, func())
}

// realClock is the Clock of the time package.
type realClock struct{}

func (realClock) Ticker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

func (realClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Config configures a Mounter.
type Config struct {
	// Rclone is the pinned rclone binary.
	Rclone string
	// Supervisor runs rclone and the FUSE helper.
	Supervisor Starter
	// Grace bounds how long rclone may take to unmount and exit once asked
	// to; zero means DefaultGrace.
	Grace time.Duration
	// Poll is how often Mount checks whether the folder is mounted; zero
	// means DefaultPoll.
	Poll time.Duration
	// Clock makes the timers; nil means the time package's.
	Clock Clock
	// Logger receives diagnostics (never secrets); nil discards them.
	Logger *slog.Logger
}

// Mounter mounts remotes and sweeps mount points. It holds no per-mount
// state and is safe for concurrent use.
type Mounter struct {
	cfg  Config
	plat platform
}

// platform is what differs between operating systems; tests replace it.
type platform struct {
	// supported is "" where mounting works at all, else why not.
	supported string
	// static runs the static checks of Detect: "" when they pass, else
	// why not.
	static func() string
	// sweep removes what a killed run left at the mount point: a stale
	// mount (Linux) or an empty leftover folder (Windows).
	sweep func(ctx context.Context, point string) error
	// prepare readies the mount point after the sweep.
	prepare func(point string) error
	// mounted reports whether the folder is mounted now.
	mounted func(point string) (bool, error)
	// cleanup runs after rclone ended: it unmounts a folder that is still
	// mounted (Linux) or removes an empty leftover folder (Windows).
	cleanup func(ctx context.Context, point string) error
	// sweepAll sweeps every mount point in the folder of mount points and
	// returns the ones it unmounted or removed.
	sweepAll func(ctx context.Context, dir string) ([]string, error)
}

// New returns a Mounter for cfg.
func New(cfg Config) (*Mounter, error) {
	switch {
	case cfg.Rclone == "":
		return nil, errors.New("mount: Config.Rclone is required")
	case cfg.Supervisor == nil:
		return nil, errors.New("mount: Config.Supervisor is required")
	}
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	m := &Mounter{cfg: cfg}
	m.plat = newPlatform(m)
	return m, nil
}

// Request describes one mount.
type Request struct {
	// Remote is the rclone path to mount ("alias:path"), already
	// validated by the caller.
	Remote string
	// Point is the absolute mount point.
	Point string
	// DevName names the mount in the system's mount table (--devname).
	DevName string
	// Flags are further rclone flags, each "--name=value", e.g.
	// --filter-from and --bwlimit.
	Flags []string
	// Env is added to rclone's environment: the remotes
	// (RCLONE_CONFIG_*) and the isolation variables.
	Env map[string]string
	// Dir is rclone's working directory.
	Dir string
	// Timeout bounds the wait until the folder is mounted.
	Timeout time.Duration
	// Scrub scrubs rclone's messages of the remotes' secrets; nil keeps
	// them as they are.
	Scrub *scrub.Scrubber
}

// Error reports a mount that could not be made: rclone exited before the
// folder was mounted, the folder was not mounted in time, or mounting is
// not possible here.
type Error struct {
	// Unavailable reports that FUSE or WinFsp is missing or refused the
	// mount, or that mounting is not supported on this system: the
	// machine cannot mount, as opposed to this mount failing.
	Unavailable bool
	// Msg is the explanation, scrubbed of secrets.
	Msg string
	// Err is the underlying error, if any.
	Err error
}

// Error implements error.
func (e *Error) Error() string { return e.Msg }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// Args returns rclone's arguments for req: the read-only mount without a
// VFS cache, with directory and attribute caches that last as long as the
// mount (it lives for one backup and nothing changes it), no polling for
// changes, JSON log lines, and req.Flags; the remote and the mount point
// follow "--".
func Args(req Request) []string {
	args := []string{
		"mount",
		"--config=",
		"--read-only",
		"--vfs-cache-mode=off",
		"--dir-cache-time=1000h",
		"--poll-interval=0",
		"--attr-timeout=1h",
		"--use-json-log",
		"--log-level=NOTICE",
	}
	if req.DevName != "" {
		args = append(args, "--devname="+req.DevName)
	}
	args = append(args, req.Flags...)
	return append(args, "--", req.Remote, req.Point)
}

// Mount is a mounted folder. Unmount it when done.
type Mount struct {
	m      *Mounter
	point  string
	cancel context.CancelFunc
	done   <-chan runOutcome
	once   sync.Once
	err    error
}

// runOutcome is how rclone's run ended.
type runOutcome struct {
	res proc.Result
	err error
}

// Point returns the mount point.
func (mt *Mount) Point() string { return mt.point }

// Mount mounts req.Remote at req.Point and returns once the folder is
// mounted. The mount ends when ctx ends (rclone is stopped), so pass the
// job's context; Unmount ends it sooner and waits until it has. Errors are
// *Error, or ctx's cause once ctx ended (rclone stopped and the mount
// point cleaned up).
func (m *Mounter) Mount(ctx context.Context, req Request) (*Mount, error) {
	if m.plat.supported != "" {
		return nil, &Error{Unavailable: true, Msg: m.plat.supported}
	}
	if !filepath.IsAbs(req.Point) {
		return nil, fmt.Errorf("mount: the mount point %q is not absolute", req.Point)
	}
	if err := m.plat.sweep(ctx, req.Point); err != nil {
		return nil, &Error{Msg: "cannot clear the mount point of an earlier run: " + err.Error(), Err: err}
	}
	if err := m.plat.prepare(req.Point); err != nil {
		return nil, &Error{Msg: "cannot prepare the mount point: " + err.Error(), Err: err}
	}

	t := rclonetool.NewTracker(nil, req.Scrub)
	t.Observe = func(l rclonetool.LogLine) {
		// The FUSE helper's own message comes as a notice, before
		// rclone's critical "failed to mount FUSE fs".
		if strings.HasPrefix(l.Msg, "mount helper error:") {
			t.Note(l.Msg)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan runOutcome, 1)
	go func() {
		res, err := m.cfg.Supervisor.Run(runCtx, proc.Spec{
			Path:        m.cfg.Rclone,
			Args:        Args(req),
			Dir:         req.Dir,
			Env:         req.Env,
			Stdout:      t.Line,
			Stderr:      t.Line,
			GracePeriod: m.cfg.Grace,
		})
		done <- runOutcome{res: res, err: err}
	}()
	mt := &Mount{m: m, point: req.Point, cancel: cancel, done: done}

	tick, stopTick := m.cfg.Clock.Ticker(m.cfg.Poll)
	defer stopTick()
	expired, stopTimer := m.cfg.Clock.After(req.Timeout)
	defer stopTimer()
	for {
		select {
		case <-ctx.Done():
			_ = mt.Unmount()
			return nil, context.Cause(ctx)
		case out := <-done:
			// rclone ended before the folder was mounted. Put the outcome
			// back for Unmount, which cleans up after it.
			done <- out
			_ = mt.Unmount()
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			return nil, exitedError(out, t.Diagnostics())
		case <-expired:
			_ = mt.Unmount()
			msg := fmt.Sprintf("the remote was not mounted within %s (remote_sources.mount_timeout)", req.Timeout)
			if d := t.Diagnostics(); len(d) > 0 {
				msg += ": " + strings.Join(d, "; ")
			}
			return nil, &Error{Unavailable: unavailable(t.Diagnostics()), Msg: msg}
		case <-tick:
			ok, err := m.plat.mounted(req.Point)
			if err != nil {
				m.cfg.Logger.Warn("cannot tell whether a remote is mounted yet", "mount_point", req.Point, "error", err.Error())
			}
			if ok {
				return mt, nil
			}
		}
	}
}

// exitedError describes an rclone mount that exited before the folder was
// mounted.
func exitedError(out runOutcome, diag []string) error {
	var b strings.Builder
	b.WriteString("rclone mount ")
	switch {
	case out.err == nil:
		b.WriteString("exited before the folder was mounted")
	case out.res.ExitCode >= 0:
		fmt.Fprintf(&b, "failed (exit code %d)", out.res.ExitCode)
	default:
		b.WriteString("failed")
	}
	if len(diag) > 0 {
		b.WriteString(": ")
		b.WriteString(strings.Join(diag, "; "))
	} else if out.err != nil {
		if _, ok := errors.AsType[*proc.StartError](out.err); ok {
			b.WriteString(": ")
			b.WriteString(out.err.Error())
		}
	}
	_, startFailed := errors.AsType[*proc.StartError](out.err)
	return &Error{Unavailable: startFailed || unavailable(diag), Msg: b.String(), Err: out.err}
}

// unavailable reports whether rclone's messages say that FUSE or WinFsp is
// missing or refused the mount (the FUSE helper failed, the FUSE device is
// missing, WinFsp cannot be found), as opposed to a failure of this mount.
func unavailable(diag []string) bool {
	for _, d := range diag {
		l := strings.ToLower(d)
		for _, s := range []string{"mount helper error", "fusermount", "/dev/fuse", "fuse device", "winfsp", "cgofuse"} {
			if strings.Contains(l, s) {
				return true
			}
		}
	}
	return false
}

// Unmount stops rclone and waits until it has ended, then makes sure the
// folder is not mounted any more (Linux: a lazy unmount with the FUSE
// helper) and cleans up the mount point. It returns an error only when the
// folder may still be mounted or the mount point could not be cleaned up.
// Later calls return the first result.
func (mt *Mount) Unmount() error {
	mt.once.Do(func() {
		mt.cancel()
		out := <-mt.done
		if out.err != nil && !isStop(out.err) {
			mt.m.cfg.Logger.Info("rclone mount ended with an error", "mount_point", mt.point, "error", out.err.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
		defer cancel()
		mt.err = mt.m.plat.cleanup(ctx, mt.point)
	})
	return mt.err
}

// isStop reports whether err only says that rclone was stopped as asked.
func isStop(err error) bool {
	_, ok := errors.AsType[*proc.CanceledError](err)
	return ok
}

// Sweep removes what a killed run left at the mount point: a stale mount
// (Linux) or an empty leftover folder (Windows). The worker calls it
// before each mount (Mount does) and for every mount point at startup
// (SweepAll).
func (m *Mounter) Sweep(ctx context.Context, point string) error {
	if m.plat.supported != "" {
		return nil
	}
	return m.plat.sweep(ctx, point)
}

// SweepAll sweeps every mount point in dir, the folder of mount points
// (sourcestate.Store.MountsDir), and returns the ones it unmounted (Linux)
// or removed (Windows). The worker calls it at startup, before anything
// walks its data directory. On Linux it reads only the mount table: it
// never lists dir or stats anything in it.
func (m *Mounter) SweepAll(ctx context.Context, dir string) ([]string, error) {
	if m.plat.supported != "" {
		return nil, nil
	}
	return m.plat.sweepAll(ctx, dir)
}

// static runs the platform's static checks (Detect).
func (m *Mounter) static() string {
	if m.plat.static == nil {
		return ""
	}
	return m.plat.static()
}
