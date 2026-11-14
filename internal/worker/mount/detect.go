package mount

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// TrialTimeout bounds the wait for the trial mount at startup.
const TrialTimeout = 5 * time.Second

// TrialPointName is the name of the trial mount's mount point in the
// folder of mount points. It is never a source key (those are hex digits
// only), and the startup sweep covers it like every mount point.
const TrialPointName = "trial"

// MaxReasonBytes bounds a reason (RemoteSourceSupport's reasons are one
// line, and the controller keeps at most 256 bytes).
const MaxReasonBytes = 256

// ReasonOff is the reason when the configuration turns the mount off.
const ReasonOff = "remote_sources.mount is off in worker.yaml"

// DetectOptions configures Detect.
type DetectOptions struct {
	// Off reports that the configuration turns the mount off
	// (remote_sources.mount: off): Detect then checks nothing.
	Off bool
	// Trial runs the trial mount once the static checks pass
	// (remote_sources.mount: auto).
	Trial bool
	// MountsDir is the folder of mount points; the trial mounts at
	// TrialPointName in it.
	MountsDir string
	// ScratchDir is a folder the trial may use (the empty folder it
	// mounts, rclone's cache and temp folders); the trial removes what it
	// created there.
	ScratchDir string
}

// Detect finds out whether this worker can mount remotes, and why not when
// it cannot, in one line of at most MaxReasonBytes (RemoteSourceSupport):
//
//  1. remote_sources.mount: off says no without checking anything;
//  2. a system other than Linux and Windows cannot mount;
//  3. the static checks: on Linux /dev/fuse opens read-write, fusermount3
//     (or fusermount) is on PATH and, when the worker does not run as root,
//     the helper is setuid root and the process may gain privileges
//     (NoNewPrivs 0 in /proc/self/status); on Windows WinFsp is installed;
//  4. with opts.Trial, a trial mount: an empty folder (":local:") is
//     mounted read-only at TrialPointName in opts.MountsDir, within
//     TrialTimeout, then unmounted. It catches what the static checks
//     cannot see: a container without the right to mount (Docker without
//     SYS_ADMIN, AppArmor's default profile, LXC).
func (m *Mounter) Detect(ctx context.Context, opts DetectOptions) (ok bool, reason string) {
	switch {
	case opts.Off:
		return false, ReasonOff
	case m.plat.supported != "":
		return false, m.plat.supported
	}
	if r := m.static(); r != "" {
		return false, Reason(r)
	}
	if !opts.Trial {
		return true, ""
	}
	if err := m.trial(ctx, opts); err != nil {
		if ctx.Err() != nil {
			return false, "the worker stopped before it could try a mount"
		}
		return false, Reason("a trial mount failed: " + err.Error())
	}
	return true, ""
}

// trial mounts an empty folder read-only and unmounts it again.
func (m *Mounter) trial(ctx context.Context, opts DetectOptions) error {
	if opts.MountsDir == "" || opts.ScratchDir == "" {
		return errors.New("mount: DetectOptions.MountsDir and ScratchDir are required for the trial")
	}
	if err := os.MkdirAll(opts.ScratchDir, 0o700); err != nil {
		return fmt.Errorf("create the trial's folder: %w", err)
	}
	dir, err := os.MkdirTemp(opts.ScratchDir, "mount-trial-*")
	if err != nil {
		return fmt.Errorf("create the trial's folder: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			m.cfg.Logger.Warn("cannot remove the trial mount's folder", "dir", dir, "error", err.Error())
		}
	}()
	source := filepath.Join(dir, "empty")
	for _, d := range []string{source, filepath.Join(dir, "cache"), filepath.Join(dir, "tmp")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fmt.Errorf("create the trial's folder: %w", err)
		}
	}
	if err := os.MkdirAll(opts.MountsDir, 0o700); err != nil {
		return fmt.Errorf("create the folder of mount points: %w", err)
	}
	env := map[string]string{
		"RCLONE_CACHE_DIR":    filepath.Join(dir, "cache"),
		"RCLONE_TEMP_DIR":     filepath.Join(dir, "tmp"),
		"RCLONE_ASK_PASSWORD": "false",
	}
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		env[name] = filepath.Join(dir, "tmp")
	}
	mt, err := m.Mount(ctx, Request{
		// ":local:" is rclone's local backend without a configured remote;
		// the path is the worker's own empty folder.
		Remote:  ":local:" + source,
		Point:   filepath.Join(opts.MountsDir, TrialPointName),
		DevName: "syncr-trial",
		Env:     env,
		Dir:     opts.ScratchDir,
		Timeout: TrialTimeout,
	})
	if err != nil {
		return err
	}
	if err := mt.Unmount(); err != nil {
		return fmt.Errorf("the trial mount worked but could not be unmounted: %w", err)
	}
	return nil
}

// Reason makes s a reason: one line of valid text of at most
// MaxReasonBytes.
func Reason(s string) string {
	s = strings.Join(strings.Fields(runner.ValidText(s)), " ")
	if len(s) <= MaxReasonBytes {
		return s
	}
	cut := MaxReasonBytes - len("...")
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}

// noNewPrivs reports whether the content of /proc/<pid>/status says the
// process may not gain privileges ("NoNewPrivs: 1"), and whether the line
// was there at all (kernels before 4.10 lack it).
func noNewPrivs(status []byte) (set, found bool) {
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		value, ok := strings.CutPrefix(sc.Text(), "NoNewPrivs:")
		if !ok {
			continue
		}
		return strings.TrimSpace(value) != "0", true
	}
	return false, false
}
