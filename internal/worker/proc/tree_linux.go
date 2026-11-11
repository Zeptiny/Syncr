//go:build linux

package proc

import (
	"errors"
	"log/slog"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// tree is the process tree of one run: the child, which leads a process
// group of its own (configureCmd), and the processes it started, which stay
// in that group unless they leave it.
type tree struct{}

func newTree() *tree { return &tree{} }

// wait waits until the child has exited, kills what is left of its process
// group, and only then reaps the child (cmd.Wait), which also collects its
// output. Killing the rest first also closes the output pipes a grandchild
// inherited, so Run does not wait out the grace period for them.
//
// Why the group signal cannot reach an unrelated process: the group's ID is
// the child's PID. waitid with WNOWAIT returns once the child has exited
// but leaves it a zombie, and a zombie keeps its PID until it is reaped. So
// when kill(-pid) runs, that number still belongs to the exited child and
// no new process can have received it; the only processes with that group
// ID are the child itself and those that joined its group, which can only
// be its descendants (setpgid only joins a group of the caller's own
// session, and a new group takes its leader's PID). The child is reaped
// only by cmd.Wait below, on this goroutine, after the signal: nothing else
// in the worker waits for children. If waitid fails (ENOSYS on some
// emulated kernels, or ECHILD should SIGCHLD ever be ignored and the child
// reaped by the kernel), the group is not signaled at all.
func (*tree) wait(cmd *exec.Cmd, log *slog.Logger) error {
	pid := cmd.Process.Pid
	if exitedUnreaped(pid) {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.Warn("cannot stop the processes a tool left running", "pid", pid, "error", err.Error())
		}
	}
	return cmd.Wait()
}

// exitedUnreaped blocks until the child pid has exited and reports whether
// it is now an unreaped zombie. The caller must be the only one that reaps
// pid, and must not have reaped it yet, so pid cannot name another process.
func exitedUnreaped(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err == nil
		}
	}
}
