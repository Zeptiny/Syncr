//go:build windows

package proc

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tree is the process tree of one run: a Job Object that holds the child
// and, because a job's processes pass their membership on to the processes
// they create, everything the child starts. Handles, unlike PIDs, cannot be
// reused while they are open, so terminating the job reaches only those
// processes.
type tree struct {
	// mu guards the handles: cancel may run on exec's context watcher while
	// wait releases them.
	mu sync.Mutex
	// job is the run's Job Object (kill-on-close); zero if there is none or
	// once it was released.
	job windows.Handle
	// proc is a handle to the child for waiting on its exit; set with job.
	proc windows.Handle
}

func newTree() *tree { return &tree{} }

// attach creates the run's Job Object and adds the started child pid to it.
// The PID cannot have been reused: exec.Cmd holds a handle to the process
// until it has been waited for.
func (t *tree) attach(pid int) (err error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	defer func() {
		if err != nil {
			_ = windows.CloseHandle(job)
		}
	}()
	// Kill-on-close as a second line: should terminating the job fail,
	// closing its handle still ends its processes.
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: the API takes a pointer to the struct
		uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("set job object limits: %w", err)
	}
	p, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // G115: a PID fits in a DWORD
	if err != nil {
		return fmt.Errorf("open process: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, p); err != nil {
		_ = windows.CloseHandle(p)
		return fmt.Errorf("assign process to job object: %w", err)
	}
	t.mu.Lock()
	t.job, t.proc = job, p
	t.mu.Unlock()
	return nil
}

// cancel is the run's Cmd.Cancel: it terminates the run's job, so the child
// and every process it started end at once. Without a job it kills the
// child only. It returns os.ErrProcessDone if the child had already exited,
// as Process.Kill does, so a run that ended on its own is not reported as
// cancelled.
func (t *tree) cancel(cmd *exec.Cmd) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return cmd.Process.Kill()
	}
	if ev, err := windows.WaitForSingleObject(t.proc, 0); err == nil && ev == windows.WAIT_OBJECT_0 {
		return os.ErrProcessDone
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		// Stop the child at least; release tries the job again once it
		// has exited, and closing the job's handle kills the rest.
		return cmd.Process.Kill()
	}
	return nil
}

// wait waits until the child has exited, terminates the run's job (what is
// left of the tree, which also closes the output pipes a grandchild
// inherited) and closes it, then collects the child (cmd.Wait).
func (t *tree) wait(cmd *exec.Cmd, log *slog.Logger) error {
	t.mu.Lock()
	p := t.proc // only release closes it, and only this goroutine calls release
	t.mu.Unlock()
	if p != 0 {
		_, _ = windows.WaitForSingleObject(p, windows.INFINITE)
		t.release(log)
	}
	return cmd.Wait()
}

// release terminates the run's job and closes its handles.
func (t *tree) release(log *slog.Logger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		log.Warn("cannot stop the processes a tool left running", "error", err.Error())
	}
	_ = windows.CloseHandle(t.job)
	_ = windows.CloseHandle(t.proc)
	t.job, t.proc = 0, 0
}
