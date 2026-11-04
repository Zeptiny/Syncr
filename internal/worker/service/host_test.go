package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProgramStartStop(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	started := make(chan struct{})
	p := newProgram(t.Context(), &stderr, func(ctx context.Context, log io.Writer) error {
		_, _ = log.Write([]byte("hello\n"))
		close(started)
		<-ctx.Done()
		return nil
	}, false, func(int) { t.Error("exit called") })
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.Start(nil); err == nil {
		t.Error("second Start accepted")
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.result(); err != nil {
		t.Errorf("result = %v", err)
	}
	if stderr.String() != "hello\n" {
		t.Errorf("fn log went to %q", stderr.String())
	}
}

func TestProgramEarlyFailureOnUnix(t *testing.T) {
	t.Parallel()
	boom := errors.New("tools missing")
	p := newProgram(t.Context(), &bytes.Buffer{}, func(context.Context, io.Writer) error { return boom },
		false, func(int) { t.Error("exit must not be called outside the Windows SCM") })
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done: // RunWait returns, so Run returns and the caller gets the error
	case <-time.After(5 * time.Second):
		t.Fatal("done not closed after fn returned")
	}
	if !errors.Is(p.result(), boom) {
		t.Errorf("result = %v", p.result())
	}
}

func TestProgramEarlyFailureUnderSCMExitsWithoutStopping(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		codes []int
	)
	exited := make(chan struct{})
	p := newProgram(t.Context(), &bytes.Buffer{}, func(context.Context, io.Writer) error {
		return errors.New("config is invalid")
	}, true, func(code int) {
		mu.Lock()
		codes = append(codes, code)
		mu.Unlock()
		close(exited)
	})
	// The SCM logger is unavailable in tests (s == nil), so stderr is used.
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit; the SCM would keep a dead service in the running state")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(codes) != 1 || codes[0] != 1 {
		t.Errorf("exit codes = %v, want [1]", codes)
	}
}

func TestProgramStopDoesNotExitUnderSCM(t *testing.T) {
	t.Parallel()
	p := newProgram(t.Context(), &bytes.Buffer{}, func(ctx context.Context, _ io.Writer) error {
		<-ctx.Done()
		return nil
	}, true, func(int) { t.Error("a requested stop must report STOPPED, not exit") })
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
}

func TestProgramParentCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	p := newProgram(ctx, &bytes.Buffer{}, func(ctx context.Context, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}, false, func(int) {})
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	cancel() // SIGTERM reaches the worker through the parent context
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fn did not stop with the parent context")
	}
}

type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) add(level string, v ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+": "+strings.TrimSpace(strings.Join(toStrings(v), " ")))
	return nil
}

func toStrings(v []any) []string {
	out := make([]string, len(v))
	for i, x := range v {
		out[i], _ = x.(string)
	}
	return out
}

func (r *recordingLogger) Error(v ...any) error          { return r.add("E", v...) }
func (r *recordingLogger) Warning(v ...any) error        { return r.add("W", v...) }
func (r *recordingLogger) Info(v ...any) error           { return r.add("I", v...) }
func (r *recordingLogger) Errorf(string, ...any) error   { return nil }
func (r *recordingLogger) Warningf(string, ...any) error { return nil }
func (r *recordingLogger) Infof(string, ...any) error    { return nil }

func TestLogWriterMapsLevelsAndKeepsLines(t *testing.T) {
	t.Parallel()
	rec := &recordingLogger{}
	w := NewLogWriter(rec)
	input := "time=2026-01-01T00:00:00Z level=INFO msg=started\n" +
		"time=2026-01-01T00:00:01Z level=WARN msg=slow\n" +
		`{"time":"2026-01-01T00:00:02Z","level":"ERROR","msg":"bad"}` + "\n" +
		"time=2026-01-01T00:00:03Z level=DEBUG msg=" // split across writes
	if _, err := w.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("tail\n\n")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"I: time=2026-01-01T00:00:00Z level=INFO msg=started",
		"W: time=2026-01-01T00:00:01Z level=WARN msg=slow",
		`E: {"time":"2026-01-01T00:00:02Z","level":"ERROR","msg":"bad"}`,
		"I: time=2026-01-01T00:00:03Z level=DEBUG msg=tail",
	}
	if strings.Join(rec.lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(rec.lines, "\n"), strings.Join(want, "\n"))
	}
}
