package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	kservice "github.com/kardianos/service"
)

// Host lets "syncr-worker run" cooperate with the service manager that
// started it.
//
// On Windows a service process must connect to the Service Control Manager
// (SCM) within seconds and answer its stop requests, otherwise the start
// fails with error 1053; that is what kardianos' Run does. Under systemd
// nothing special is required, but going through the same code keeps one
// path for every platform.
type Host interface {
	// Managed reports whether the process was started by a service manager.
	// Interactive runs (a terminal, a container, a scheduled task) are not.
	Managed() bool
	// Run runs fn under the service manager and blocks until the service is
	// stopped or fn returns. ctx is cancelled when the manager asks the
	// service to stop (or when the parent context ends); fn must then return
	// promptly. log is where fn writes its log lines: the Windows Event Log
	// under the SCM, otherwise stderr. If fn fails, the error is also written
	// to log, and Run returns it.
	Run(ctx context.Context, name string, stderr io.Writer, fn func(ctx context.Context, log io.Writer) error) error
}

// NewHost returns the Host backed by kardianos/service.
func NewHost() Host { return kardianosHost{} }

type kardianosHost struct{}

func (kardianosHost) Managed() bool { return !kservice.Interactive() }

// stopTimeout bounds how long a stop request waits for the worker to finish
// shutting down. The Windows SCM gives a service about 20 seconds by default.
const stopTimeout = 20 * time.Second

func (kardianosHost) Run(ctx context.Context, name string, stderr io.Writer, fn func(context.Context, io.Writer) error) error {
	p := newProgram(ctx, stderr, fn, runtime.GOOS == "windows", os.Exit)
	svc, err := kservice.New(p, &kservice.Config{
		Name: name,
		// On Unix, Run returns when the program ends; on Windows the SCM
		// drives the lifetime and RunWait is unused.
		Option: kservice.KeyValue{"RunWait": func() { <-p.done }},
	})
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	runErr := svc.Run()
	if err := p.result(); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("service manager: %w", runErr)
	}
	return nil
}

// program adapts a function to kservice.Interface.
type program struct {
	parent  context.Context
	stderr  io.Writer
	fn      func(context.Context, io.Writer) error
	scm     bool      // true on Windows, where the SCM must see a crash
	exit    func(int) // os.Exit; replaced in tests
	cancel  context.CancelFunc
	done    chan struct{}
	started atomic.Bool

	stopping atomic.Bool
	mu       sync.Mutex
	err      error
}

func newProgram(ctx context.Context, stderr io.Writer, fn func(context.Context, io.Writer) error, scm bool, exit func(int)) *program {
	return &program{parent: ctx, stderr: stderr, fn: fn, scm: scm, exit: exit, done: make(chan struct{})}
}

// Start implements kservice.Interface. It must return quickly, so the worker
// runs in a goroutine.
func (p *program) Start(s kservice.Service) error {
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("service already started")
	}
	ctx, cancel := context.WithCancel(p.parent)
	p.cancel = cancel
	logw := p.logWriter(s)
	go func() {
		defer close(p.done)
		err := p.fn(ctx, logw)
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		if p.scm && err != nil {
			// The caller only sees this error after the SCM is gone; the
			// Event Log is where an operator will look.
			_, _ = fmt.Fprintf(logw, "level=ERROR msg=%q\n", "worker stopped: "+err.Error())
		}
		if !p.stopping.Load() && p.scm {
			// The worker ended by itself. Leave without reporting STOPPED, so
			// the SCM treats it as a crash and applies the service's
			// recovery action (restart).
			code := 0
			if err != nil {
				code = 1
			}
			p.exit(code)
		}
	}()
	return nil
}

// Stop implements kservice.Interface.
func (p *program) Stop(kservice.Service) error {
	p.stopping.Store(true)
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(stopTimeout):
		return fmt.Errorf("the worker did not stop within %s", stopTimeout)
	}
}

func (p *program) result() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// logWriter returns the Event Log writer under the Windows SCM and stderr
// elsewhere (journald captures it under systemd).
func (p *program) logWriter(s kservice.Service) io.Writer {
	if !p.scm || s == nil {
		return p.stderr
	}
	l, err := s.Logger(nil)
	if err != nil {
		return p.stderr
	}
	return NewLogWriter(l)
}
