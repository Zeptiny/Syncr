//go:build linux

package proc

import (
	"log/slog"
	"os/exec"
	"runtime"
	"syscall"
)

// lifetime ties children to the worker's lifetime; see startChild.
type lifetime struct{}

// startChild starts cmd and returns a channel that receives the result of
// cmd.Wait.
//
// The child is killed (SIGKILL) when the worker dies, through the parent
// death signal, so a crashed or killed worker never leaves a tool running
// that keeps writing to a remote or repository, or that a restarted worker
// would race with when it runs the job again.
//
// The kernel sends that signal when the thread that forked the child exits,
// not when the process does, and the Go runtime ends a thread whenever a
// goroutine locked to it exits. So the child is started and waited for by a
// goroutine that keeps its OS thread locked until the child has exited: no
// other goroutine can run on that thread, and so none can retire it, while
// the child runs.
func (*lifetime) startChild(cmd *exec.Cmd, _ *slog.Logger) (<-chan error, error) {
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	started := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			runtime.UnlockOSThread()
			started <- err
			return
		}
		started <- nil
		err := cmd.Wait()
		// The child has exited and been reaped: the thread may be reused.
		runtime.UnlockOSThread()
		done <- err
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return done, nil
}
