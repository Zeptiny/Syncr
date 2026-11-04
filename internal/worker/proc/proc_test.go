package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the child process: when helperEnv is set,
// TestMain runs a helper mode instead of the tests. The variable reaches the
// child through Spec.Env, which also proves that caller variables pass the
// minimal environment.
const helperEnv = "SYNCR_PROC_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(helperEnv); ok {
		os.Exit(runHelper(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runHelper implements the fake child modes and returns its exit code.
func runHelper(mode string, args []string) int {
	switch mode {
	case "stdio": // args: exit code
		fmt.Println("out-1")
		fmt.Fprintln(os.Stderr, "err-1")
		fmt.Println("out-2")
		code, _ := strconv.Atoi(args[0])
		return code
	case "env":
		env := os.Environ()
		slices.Sort(env)
		for _, kv := range env {
			fmt.Println(kv)
		}
		return 0
	case "long": // args: line length
		n, _ := strconv.Atoi(args[0])
		fmt.Println(strings.Repeat("x", n))
		fmt.Println("after")
		return 0
	case "framing":
		fmt.Print("crlf\r\n\nlast-without-newline")
		return 0
	case "flood": // args: lines per stream
		n, _ := strconv.Atoi(args[0])
		for i := range n {
			fmt.Printf("out-%d\n", i)
			fmt.Fprintf(os.Stderr, "err-%d\n", i)
		}
		return 0
	case "block": // waits until killed or interrupted (default signal action)
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return 0
	case "pid": // prints its PID, then waits until killed
		fmt.Printf("pid=%d\n", os.Getpid())
		time.Sleep(time.Minute)
		return 0
	case "trap": // handles the stop signal and exits 3
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		fmt.Println("ready")
		select {
		case <-sig:
			fmt.Println("stopping")
			return 3
		case <-time.After(time.Minute):
			return 0
		}
	case "ignore": // ignores the stop signal; must be killed
		signal.Ignore(os.Interrupt)
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return 0
	case "pgid":
		return printProcessGroup()
	case "orphan": // leaves a grandchild holding stdout open, then exits 0
		self, err := os.Executable()
		if err != nil {
			return 10
		}
		cmd := exec.Command(self) // deliberately outlives this helper
		cmd.Env = append(os.Environ(), helperEnv+"=block")
		cmd.Stdout = os.Stdout
		if err := cmd.Start(); err != nil {
			return 11
		}
		fmt.Printf("grandchild=%d\n", cmd.Process.Pid)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 99
}

// helperSpec returns a Spec that runs this test binary in the given mode.
func helperSpec(t *testing.T, mode string, args ...string) Spec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{helperEnv: mode}
	if dir, ok := os.LookupEnv("GOCOVERDIR"); ok {
		env["GOCOVERDIR"] = dir // keep coverage builds quiet
	}
	return Spec{Path: self, Args: args, Env: env}
}

// collector records lines; it is deliberately unsynchronized so the race
// detector flags any concurrent callback invocation.
type collector struct {
	stdout, stderr []string
	truncated      []bool
}

func (c *collector) attach(spec *Spec) {
	spec.Stdout = func(l Line) {
		c.stdout = append(c.stdout, string(l.Data))
		c.truncated = append(c.truncated, l.Truncated)
	}
	spec.Stderr = func(l Line) { c.stderr = append(c.stderr, string(l.Data)) }
}

func TestRunStreamsOutput(t *testing.T) {
	t.Parallel()
	var c collector
	spec := helperSpec(t, "stdio", "0")
	c.attach(&spec)

	res, err := New(Options{}).Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.ExitCode != 0 || res.Duration <= 0 {
		t.Errorf("Result = %+v", res)
	}
	if !slices.Equal(c.stdout, []string{"out-1", "out-2"}) {
		t.Errorf("stdout = %q", c.stdout)
	}
	if !slices.Equal(c.stderr, []string{"err-1"}) {
		t.Errorf("stderr = %q", c.stderr)
	}
}

func TestRunNilCallbacksDiscard(t *testing.T) {
	t.Parallel()
	res, err := New(Options{}).Run(t.Context(), helperSpec(t, "stdio", "0"))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run() = (%+v, %v)", res, err)
	}
}

func TestRunExitError(t *testing.T) {
	t.Parallel()
	res, err := New(Options{}).Run(t.Context(), helperSpec(t, "stdio", "7"))
	exitErr, ok := errors.AsType[*ExitError](err)
	if !ok {
		t.Fatalf("Run() error = %v, want *ExitError", err)
	}
	if exitErr.ExitCode != 7 || res.ExitCode != 7 {
		t.Errorf("exit code: error %d, result %d, want 7", exitErr.ExitCode, res.ExitCode)
	}
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Error("ExitError does not unwrap to *exec.ExitError")
	}
}

func TestRunStartError(t *testing.T) {
	t.Parallel()
	missing := helperSpec(t, "stdio", "0")
	missing.Path += "-does-not-exist"
	badEnv := helperSpec(t, "stdio", "0")
	badEnv.Env["BAD=NAME"] = "x"

	tests := map[string]Spec{
		"missing binary": missing,
		"empty path":     {},
		"invalid env":    badEnv,
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			called := false
			spec.Stdout = func(Line) { called = true }
			res, err := New(Options{}).Run(t.Context(), spec)
			if _, ok := errors.AsType[*StartError](err); !ok {
				t.Fatalf("Run() error = %v, want *StartError", err)
			}
			if res.ExitCode != -1 || called {
				t.Errorf("Result = %+v, callback called = %v", res, called)
			}
		})
	}
}

func TestRunMinimalEnvironment(t *testing.T) {
	// Not parallel: it sets variables in the test process environment.
	t.Setenv("SYNCR_WORKER_TOKEN", "worker-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("TZ", "UTC")

	var c collector
	spec := helperSpec(t, "env")
	spec.Env["RCLONE_CONFIG_SRC_TYPE"] = "local"
	spec.Env["TZ"] = "Europe/Lisbon" // caller overrides an allowlisted variable
	c.attach(&spec)

	if _, err := New(Options{}).Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, kv := range c.stdout {
		name, value, _ := strings.Cut(kv, "=")
		if _, dup := got[name]; dup {
			t.Errorf("variable %s appears twice", name)
		}
		got[name] = value
	}
	for _, leaked := range []string{"SYNCR_WORKER_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := got[leaked]; ok {
			t.Errorf("worker variable %s leaked into the child", leaked)
		}
	}
	if got["RCLONE_CONFIG_SRC_TYPE"] != "local" || got[helperEnv] != "env" {
		t.Errorf("caller variables missing: %v", got)
	}
	if got["TZ"] != "Europe/Lisbon" {
		t.Errorf("TZ = %q, want caller override", got["TZ"])
	}
	for name := range got {
		switch {
		case allowedEnv(name), hasEnv(spec.Env, name):
		case runtime.GOOS == "windows" && (strings.EqualFold(name, "SYSTEMROOT") || name == ""):
			// exec adds SYSTEMROOT on Windows; hidden "=C:" drive entries have
			// an empty name.
		default:
			t.Errorf("unexpected variable %s in child environment", name)
		}
	}
}

func TestRunLongLines(t *testing.T) {
	t.Parallel()
	var c collector
	spec := helperSpec(t, "long", strconv.Itoa(2*DefaultMaxLineBytes+123))
	c.attach(&spec)

	if _, err := New(Options{}).Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if len(c.stdout) != 2 {
		t.Fatalf("got %d lines, want 2", len(c.stdout))
	}
	if len(c.stdout[0]) != DefaultMaxLineBytes || !c.truncated[0] {
		t.Errorf("first line: %d bytes, truncated=%v; want %d bytes, truncated", len(c.stdout[0]), c.truncated[0], DefaultMaxLineBytes)
	}
	if c.stdout[1] != "after" || c.truncated[1] {
		t.Errorf("second line = %q (truncated=%v), want \"after\"", c.stdout[1], c.truncated[1])
	}
}

func TestRunLineFraming(t *testing.T) {
	t.Parallel()
	var c collector
	spec := helperSpec(t, "framing")
	c.attach(&spec)

	if _, err := New(Options{}).Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if want := []string{"crlf", "", "last-without-newline"}; !slices.Equal(c.stdout, want) {
		t.Errorf("stdout = %q, want %q", c.stdout, want)
	}
}

func TestRunDeliversEveryLineInOrder(t *testing.T) {
	t.Parallel()
	const n = 5000
	var c collector
	spec := helperSpec(t, "flood", strconv.Itoa(n))
	c.attach(&spec)

	if _, err := New(Options{}).Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if len(c.stdout) != n || len(c.stderr) != n {
		t.Fatalf("got %d stdout and %d stderr lines, want %d each", len(c.stdout), len(c.stderr), n)
	}
	for i := range n {
		if c.stdout[i] != fmt.Sprintf("out-%d", i) || c.stderr[i] != fmt.Sprintf("err-%d", i) {
			t.Fatalf("line %d out of order: %q / %q", i, c.stdout[i], c.stderr[i])
		}
	}
}

// cancelOnReady returns a spec whose first stdout line "ready" cancels the
// returned context with cause.
func cancelOnReady(t *testing.T, mode string, cause error) (context.Context, Spec, *[]string) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(nil) })
	spec := helperSpec(t, mode)
	var lines []string
	spec.Stdout = func(l Line) {
		lines = append(lines, string(l.Data))
		if string(l.Data) == "ready" {
			cancel(cause)
		}
	}
	return ctx, spec, &lines
}

func TestRunCancel(t *testing.T) {
	t.Parallel()
	errJobCancelled := errors.New("job cancelled by user")
	ctx, spec, _ := cancelOnReady(t, "block", errJobCancelled)

	start := time.Now()
	res, err := New(Options{GracePeriod: 5 * time.Second}).Run(ctx, spec)
	elapsed := time.Since(start)

	canceled, ok := errors.AsType[*CanceledError](err)
	if !ok {
		t.Fatalf("Run() error = %v, want *CanceledError", err)
	}
	if !errors.Is(err, errJobCancelled) {
		t.Errorf("error %v does not unwrap to the cancellation cause", err)
	}
	if canceled.ExitCode != res.ExitCode || res.ExitCode == 0 {
		t.Errorf("exit codes: error %d, result %d; want equal and non-zero", canceled.ExitCode, res.ExitCode)
	}
	// "block" keeps the default signal action, so the stop request ends it
	// well before the grace period.
	if elapsed > 4*time.Second {
		t.Errorf("Run took %v after cancellation", elapsed)
	}
}

func TestRunAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := New(Options{}).Run(ctx, helperSpec(t, "stdio", "0"))
	if _, ok := errors.AsType[*CanceledError](err); !ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want *CanceledError wrapping context.Canceled", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

func TestRunOrphanedPipesDoNotHang(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "orphan")
	spec.GracePeriod = 300 * time.Millisecond
	spec.Stdout = func(l Line) {
		if pid, ok := strings.CutPrefix(string(l.Data), "grandchild="); ok {
			if n, err := strconv.Atoi(pid); err == nil {
				t.Cleanup(func() {
					if p, err := os.FindProcess(n); err == nil {
						_ = p.Kill()
					}
				})
			}
		}
	}

	start := time.Now()
	res, err := New(Options{}).Run(t.Context(), spec)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %v; pipes held by the grandchild were not closed", elapsed)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("Run() error = %v, want exec.ErrWaitDelay", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestRunCallbackPanicPropagates(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "block")
	spec.Stdout = func(Line) { panic("boom") }

	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v, want boom", r)
		}
	}()
	_, _ = New(Options{}).Run(t.Context(), spec)
	t.Fatal("Run returned instead of panicking")
}
