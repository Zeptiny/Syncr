//go:build windows

package proc

import (
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// lifetime ties children to the worker's lifetime through a Job Object.
type lifetime struct {
	once sync.Once
	job  windows.Handle
	err  error
}

// startChild starts cmd, adds it to the supervisor's Job Object and returns
// a channel that receives the result of cmd.Wait.
//
// The Job Object is created once per Supervisor (the worker has one) with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, and its handle is never closed: when
// the worker exits for any reason, including a crash or TerminateProcess,
// Windows closes the handle and kills every process in the job, so no tool
// keeps writing to a remote or repository, or races with a restarted worker
// that runs the job again. Processes a child starts (restic's rclone) are in
// the job too. While the worker runs, the job changes nothing: children are
// still stopped by Run, and grandchildren may outlive their parent.
//
// os/exec cannot create the child suspended, so it is added to the job just
// after it started: if the worker dies in that short window, or the child
// starts a process within it, that process is not killed with the worker.
//
// If the job cannot be created or the child cannot be added (for example a
// worker inside a job that does not allow nesting), the child runs anyway,
// without that guarantee, and a warning is logged.
//
// The child is then added to a second Job Object of the run's own (t),
// which becomes nested in the worker's: canceling the run terminates that
// job, and so the child and everything it started, and so does the child's
// exit (tree.wait). The order matters: a process already in a job can only
// join an empty job, which then nests inside the first one. If the run's
// job cannot be set up, the run continues as without it (cancel kills only
// the child) and a warning is logged.
func (l *lifetime) startChild(cmd *exec.Cmd, t *tree, log *slog.Logger) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	if err := l.assign(pid); err != nil {
		log.Warn("tool process is not tied to the worker's lifetime; it keeps running if the worker is killed",
			"pid", pid, "error", err.Error())
	}
	if err := t.attach(pid); err != nil {
		log.Warn("processes a tool starts are not stopped with it; they may outlive a cancelled job",
			"pid", pid, "error", err.Error())
	}
	done := make(chan error, 1)
	go func() { done <- t.wait(cmd, log) }()
	return done, nil
}

// jobObject returns the kill-on-close Job Object, creating it on first use.
// The handle stays open for the worker's lifetime.
func (l *lifetime) jobObject() (windows.Handle, error) {
	l.once.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			l.err = fmt.Errorf("create job object: %w", err)
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: the API takes a pointer to the struct
			uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(h)
			l.err = fmt.Errorf("set job object limits: %w", err)
			return
		}
		l.job = h
	})
	return l.job, l.err
}

// assign adds the process pid to the Job Object. The PID cannot have been
// reused: exec.Cmd holds a handle to the process until it has been waited
// for.
func (l *lifetime) assign(pid int) error {
	job, err := l.jobObject()
	if err != nil {
		return err
	}
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid)) //nolint:gosec // G115: a PID fits in a DWORD
	if err != nil {
		return fmt.Errorf("open process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(p) }()
	if err := windows.AssignProcessToJobObject(job, p); err != nil {
		return fmt.Errorf("assign process to job object: %w", err)
	}
	return nil
}
