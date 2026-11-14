//go:build integration && linux

package restic

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/mount"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// TestIntegrationRemoteSourceMountKilled: rclone is killed (SIGKILL) once
// the folder is mounted, before restic reads it. Whatever restic makes of
// the dead mount, the job fails with EXECUTION_FAILED and says the mount
// ended; the dead mount is unmounted, and the next run works.
func TestIntegrationRemoteSourceMountKilled(t *testing.T) {
	requireMounter(t)
	rcloneEnded := make(chan struct{}, 1)
	m, err := mount.New(mount.Config{
		Rclone:     toolPath(t, "SYNCR_TEST_RCLONE"),
		Supervisor: endSignal{Starter: proc.New(proc.Options{}), ended: rcloneEnded},
	})
	if err != nil {
		t.Fatal(err)
	}
	kill := &atomic.Bool{}
	h := newHarnessWith(t, func(_ *harness, cfg *Config) {
		cfg.Supervisor = killMountFirst{Supervisor: cfg.Supervisor, t: t, restic: cfg.ResticPath, on: kill, ended: rcloneEnded}
	})
	remote := localSource(h)
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true, Mount: true}, itMounter{m: m})
	h.init(itPassword)
	writeFile(t, filepath.Join(remote, "photos", "a.jpg"), randomBytes(t, 3000))
	writeFile(t, filepath.Join(remote, "photos", "2024", "b.jpg"), randomBytes(t, 2000))
	viaMount := func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Method = mountMethod }
	point := filepath.Join(store.MountsDir(), sourceKey)

	kill.Store(true)
	_, err = h.remoteRun(t.Context(), "src", "photos", nil, viaMount)
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if !strings.Contains(err.Error(), "ended during the backup") || !strings.Contains(err.Error(), "rclone mount ended while the folder was mounted") {
		t.Errorf("error = %v", err)
	}
	t.Logf("killed mount: %v", err)
	if mounted(t, point) {
		t.Errorf("%s is still mounted after the run", point)
	}

	kill.Store(false)
	out := h.mustRemoteRun("src", "photos", viaMount)
	if out.GetSnapshotId() == "" || out.GetFilesNew()+out.GetFilesUnmodified()+out.GetFilesChanged() != 2 {
		t.Errorf("next run = %v", out)
	}
}

// endSignal sends on ended each time an rclone mount it ran has ended.
type endSignal struct {
	mount.Starter
	ended chan<- struct{}
}

func (s endSignal) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	res, err := s.Starter.Run(ctx, spec)
	if len(spec.Args) > 0 && spec.Args[0] == "mount" {
		select {
		case s.ended <- struct{}{}:
		default:
		}
	}
	return res, err
}

// killMountFirst SIGKILLs the rclone mount of sourceKey before restic's
// backup starts while on is set, and waits until the mount noticed.
type killMountFirst struct {
	Supervisor
	t      *testing.T
	restic string
	on     *atomic.Bool
	ended  <-chan struct{}
}

func (k killMountFirst) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	if k.on.Load() && spec.Path == k.restic && slices.Contains(spec.Args, "backup") {
		pid := mountPID(k.t, "--devname=syncr-"+sourceKey)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			k.t.Errorf("kill rclone mount %d: %v", pid, err)
		}
		select {
		case <-k.ended:
		case <-time.After(30 * time.Second):
			k.t.Error("rclone mount did not end within 30s of SIGKILL")
		}
	}
	return k.Supervisor.Run(ctx, spec)
}

// mountPID finds the process whose command line has the argument arg.
func mountPID(t *testing.T, arg string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		for _, a := range bytes.Split(cmdline, []byte{0}) {
			if string(a) == arg {
				return pid
			}
		}
	}
	t.Fatalf("no process with %s", arg)
	return 0
}

// mounted reports whether point is in this process's mount table.
func mounted(t *testing.T, point string) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == point {
			return true
		}
	}
	return false
}
