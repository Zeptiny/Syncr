package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// The test binary doubles as the hooks' command: when helperEnv is set,
// TestMain runs a helper mode instead of the tests. The variable reaches the
// command through the definition's Env, as an operator's variables do.
const helperEnv = "HOOKS_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(helperEnv); ok {
		os.Exit(runHelper(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runHelper implements the fake commands and returns their exit code.
func runHelper(mode string, args []string) int {
	switch mode {
	case "print": // args: exit code
		fmt.Println("to stdout")
		fmt.Fprintln(os.Stderr, "to stderr")
		code, _ := strconv.Atoi(args[0])
		return code
	case "args":
		for _, a := range args {
			fmt.Println(a)
		}
		return 0
	case "env":
		env := os.Environ()
		slices.Sort(env)
		for _, kv := range env {
			fmt.Println(kv)
		}
		return 0
	case "flood": // args: number of lines
		n, _ := strconv.Atoi(args[0])
		for i := range n {
			fmt.Printf("line %06d\n", i)
		}
		return 0
	case "block": // waits until it is stopped
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 99
}

// helper returns a definition that runs this test binary in mode.
func helper(t *testing.T, name, mode string, args ...string) Definition {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{helperEnv: mode}
	if dir, ok := os.LookupEnv("GOCOVERDIR"); ok {
		env["GOCOVERDIR"] = dir // keep coverage builds quiet
	}
	return Definition{Name: name, Command: append([]string{self}, args...), Timeout: 30 * time.Second, Env: env}
}

// newSet returns a Set of defs over a real supervisor with a short grace
// period.
func newSet(t *testing.T, defs ...Definition) *Set {
	t.Helper()
	s, err := New(Config{Definitions: defs, Supervisor: proc.New(proc.Options{}), GracePeriod: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewRejectsInvalidDefinitions(t *testing.T) {
	t.Parallel()
	sup := proc.New(proc.Options{})
	ok := Definition{Name: "a", Command: []string{"true"}, Timeout: time.Second}
	many := make([]Definition, link.MaxWorkerHooks+1)
	for i := range many {
		many[i] = Definition{Name: fmt.Sprintf("hook-%d", i), Command: ok.Command, Timeout: ok.Timeout}
	}
	tests := map[string]struct {
		cfg  Config
		want string
	}{
		"bad name":      {Config{Supervisor: sup, Definitions: []Definition{{Name: "Not Valid", Command: ok.Command, Timeout: ok.Timeout}}}, "not a valid hook name"},
		"empty name":    {Config{Supervisor: sup, Definitions: []Definition{{Command: ok.Command, Timeout: ok.Timeout}}}, "not a valid hook name"},
		"no command":    {Config{Supervisor: sup, Definitions: []Definition{{Name: "a", Timeout: ok.Timeout}}}, "has no command"},
		"empty program": {Config{Supervisor: sup, Definitions: []Definition{{Name: "a", Command: []string{""}, Timeout: ok.Timeout}}}, "has no command"},
		"no timeout":    {Config{Supervisor: sup, Definitions: []Definition{{Name: "a", Command: ok.Command}}}, "timeout"},
		"negative":      {Config{Supervisor: sup, Definitions: []Definition{{Name: "a", Command: ok.Command, Timeout: -1}}}, "timeout"},
		"duplicate":     {Config{Supervisor: sup, Definitions: []Definition{ok, ok}}, "defined twice"},
		"too many":      {Config{Supervisor: sup, Definitions: many}, "at most 64"},
		"no supervisor": {Config{Definitions: []Definition{ok}}, "supervisor"},
		"bad grace":     {Config{Supervisor: sup, GracePeriod: -time.Second}, "grace period"},
	}
	for name, tt := range tests {
		if _, err := New(tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: New() = %v, want an error containing %q", name, err, tt.want)
		}
	}
	// No hooks need no supervisor.
	s, err := New(Config{})
	if err != nil || len(s.Names()) != 0 || len(s.Infos()) != 0 {
		t.Errorf("New(empty) = %v, %v", s, err)
	}
}

func TestInfosDescribeHooksWithoutTheirCommands(t *testing.T) {
	t.Parallel()
	s, err := New(Config{Supervisor: proc.New(proc.Options{}), Definitions: []Definition{
		{Name: "stop-app", Command: []string{"docker", "stop", "secret-container"}, Timeout: time.Minute, Env: map[string]string{"TOKEN": "hunter2"}},
		{Name: "dump-db", Description: "Dump the database", Command: []string{"/opt/dump.sh"}, Timeout: 15 * time.Minute},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Names(); !slices.Equal(got, []string{"dump-db", "stop-app"}) {
		t.Errorf("Names() = %q, want them sorted", got)
	}
	infos := s.Infos()
	if len(infos) != 2 || infos[0].GetName() != "dump-db" || infos[0].GetDescription() != "Dump the database" ||
		infos[0].GetTimeout().AsDuration() != 15*time.Minute || infos[1].GetName() != "stop-app" || infos[1].GetTimeout().AsDuration() != time.Minute {
		t.Errorf("Infos() = %v", infos)
	}
	for _, in := range infos {
		if text := in.String(); strings.Contains(text, "secret-container") || strings.Contains(text, "hunter2") || strings.Contains(text, "dump.sh") {
			t.Errorf("the info of %s gives away its command or environment: %s", in.GetName(), text)
		}
	}
}

func TestSetIsIndependentOfItsInputs(t *testing.T) {
	t.Parallel()
	d := helper(t, "args", "args", "one")
	s := newSet(t, d)
	d.Command[1] = "changed"
	d.Env[helperEnv] = "print"
	run := s.Run(context.Background(), "args", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetOutput() != "one" {
		t.Errorf("output = %q, want the arguments the Set was built with", run.GetOutput())
	}
	names := s.Names()
	names[0] = "x"
	if s.Names()[0] != "args" {
		t.Error("Names() returned the Set's own slice")
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	s := newSet(t, helper(t, "stop-app", "print", "0"), helper(t, "start-app", "print", "0"))
	var nine []string
	for i := range link.MaxHooksPerPhase + 1 {
		nine = append(nine, fmt.Sprintf("h%d", i))
	}
	tests := []struct {
		name  string
		hooks *syncrv1.JobHooks
		want  []string // substrings; none means usable
	}{
		{name: "no hooks", hooks: nil},
		{name: "empty", hooks: &syncrv1.JobHooks{}},
		{name: "all defined", hooks: &syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}},
		{name: "one hook in several phases", hooks: &syncrv1.JobHooks{Before: []string{"stop-app"}, OnFailure: []string{"stop-app"}, After: []string{"stop-app", "start-app"}}},
		{name: "unknown", hooks: &syncrv1.JobHooks{Before: []string{"stop-app", "dump-db"}}, want: []string{`no hook named "dump-db"`, "worker's configuration"}},
		{name: "several unknown, each once", hooks: &syncrv1.JobHooks{Before: []string{"x"}, OnSuccess: []string{"y", "x"}, After: []string{"start-app"}},
			want: []string{`no hooks named "x", "y":`}},
		{name: "twice in a list", hooks: &syncrv1.JobHooks{After: []string{"start-app", "start-app"}}, want: []string{`the after hook "start-app" twice`}},
		{name: "too many", hooks: &syncrv1.JobHooks{OnFailure: nine}, want: []string{"9 on_failure hooks", "at most 8"}},
		{name: "a name that is none", hooks: &syncrv1.JobHooks{Before: []string{"a\xffb" + strings.Repeat("é", 100)}}, want: []string{"no hook named", "..."}},
	}
	for _, tt := range tests {
		err := s.Check(tt.hooks)
		if len(tt.want) == 0 {
			if err != nil {
				t.Errorf("%s: Check() = %v, want nil", tt.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: Check() = nil, want an error", tt.name)
			continue
		}
		if !utf8.ValidString(err.Error()) || len(err.Error()) > 400 {
			t.Errorf("%s: the message is not valid UTF-8 or too long (%d bytes): %q", tt.name, len(err.Error()), err)
		}
		for _, w := range tt.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not contain %q", tt.name, err, w)
			}
		}
	}

	// A worker without hooks (a nil Set included) runs jobs without hooks
	// and refuses the others.
	for _, none := range []*Set{nil, {}} {
		if err := none.Check(nil); err != nil {
			t.Errorf("Check(nil) on a Set without hooks = %v", err)
		}
		if err := none.Check(&syncrv1.JobHooks{Before: []string{"stop-app"}}); err == nil {
			t.Error("a Set without hooks accepted a job with a hook")
		}
		if none.Names() != nil && len(none.Names()) != 0 || len(none.Infos()) != 0 || len(none.Unusable()) != 0 {
			t.Error("a Set without hooks lists some")
		}
	}
}

func TestRunSucceeds(t *testing.T) {
	t.Parallel()
	s := newSet(t, helper(t, "print", "print", "0"))
	before := time.Now().Add(-time.Second)
	run := s.Run(context.Background(), "print", syncrv1.HookPhase_HOOK_PHASE_ON_SUCCESS, nil)
	if run.GetName() != "print" || run.GetPhase() != syncrv1.HookPhase_HOOK_PHASE_ON_SUCCESS ||
		run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED || run.GetExitCode() != 0 || run.GetMessage() != "" {
		t.Errorf("run = %v", run)
	}
	// Both streams are kept, in the order they were read.
	if got := strings.Split(run.GetOutput(), "\n"); len(got) != 2 || !slices.Contains(got, "to stdout") || !slices.Contains(got, "to stderr") || run.GetOutputTruncated() {
		t.Errorf("output = %q (truncated %v), want both streams", run.GetOutput(), run.GetOutputTruncated())
	}
	if at := run.GetStartedAt().AsTime(); at.Before(before) || at.After(time.Now()) {
		t.Errorf("started_at = %s, want about now", at)
	}
	if d := run.GetDuration().AsDuration(); d <= 0 || d > 30*time.Second {
		t.Errorf("duration = %s", d)
	}
}

func TestRunPassesItsArgumentsAsTheyAre(t *testing.T) {
	t.Parallel()
	// No shell: nothing in an argument is expanded or split.
	args := []string{"two words", "$HOME", "`id`", "a;b", "*", `"quoted"`, ""}
	s := newSet(t, helper(t, "args", "args", args...))
	run := s.Run(context.Background(), "args", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED {
		t.Fatalf("run = %v", run)
	}
	// One line per argument; the empty last one is an empty last line.
	if got, want := run.GetOutput(), strings.Join(args, "\n"); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestRunReportsAFailingCommand(t *testing.T) {
	t.Parallel()
	s := newSet(t, helper(t, "print", "print", "3"))
	run := s.Run(context.Background(), "print", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_FAILED || run.GetExitCode() != 3 || run.GetMessage() != "exited with code 3" {
		t.Errorf("run = %v", run)
	}
	if !strings.Contains(run.GetOutput(), "to stderr") {
		t.Errorf("output = %q, want what the command printed before it failed", run.GetOutput())
	}
}

func TestRunReportsACommandThatCannotStart(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-program")
	s := newSet(t, Definition{Name: "gone", Command: []string{missing}, Timeout: time.Second})
	run := s.Run(context.Background(), "gone", syncrv1.HookPhase_HOOK_PHASE_AFTER, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_FAILED || run.GetExitCode() != -1 || !strings.HasPrefix(run.GetMessage(), "could not be started: ") {
		t.Errorf("run = %v", run)
	}
	if run.GetStartedAt() == nil || run.GetDuration() == nil {
		t.Errorf("run = %v, want a start time and a duration", run)
	}
	if got := s.Unusable(); len(got) != 1 || got["gone"] == nil {
		t.Errorf("Unusable() = %v, want the missing program", got)
	}
}

func TestRunOfAnUndefinedHookFailsWithoutRunningAnything(t *testing.T) {
	t.Parallel()
	for _, s := range []*Set{nil, newSet(t)} {
		run := s.Run(context.Background(), "ghost", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
		if run == nil || run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_FAILED || run.GetExitCode() != -1 ||
			run.GetName() != "ghost" || !strings.Contains(run.GetMessage(), "defines no hook") {
			t.Errorf("run = %v", run)
		}
	}
}

func TestRunStopsAtTheTimeout(t *testing.T) {
	t.Parallel()
	d := helper(t, "block", "block")
	d.Timeout = 300 * time.Millisecond
	s := newSet(t, d)
	start := time.Now()
	run := s.Run(context.Background(), "block", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT || run.GetMessage() != "timed out after 300ms" {
		t.Errorf("run = %v", run)
	}
	if run.GetOutput() != "ready" {
		t.Errorf("output = %q, want what it printed before it was stopped", run.GetOutput())
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("the run took %s, want the timeout plus at most the grace period", took)
	}
}

func TestRunStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	s := newSet(t, helper(t, "block", "block"))
	cause := errors.New("job cancelled by the controller")
	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(300*time.Millisecond, func() { cancel(cause) })
	run := s.Run(ctx, "block", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_CANCELLED || run.GetMessage() != "stopped: job cancelled by the controller" {
		t.Errorf("run = %v", run)
	}

	// A context that is already done starts nothing.
	run = s.Run(ctx, "block", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_CANCELLED || run.GetOutput() != "" || !strings.Contains(run.GetMessage(), "job cancelled by the controller") {
		t.Errorf("run with a done context = %v", run)
	}
}

func TestRunKeepsTheEndOfLongOutput(t *testing.T) {
	t.Parallel()
	s := newSet(t, helper(t, "flood", "flood", "5000"))
	run := s.Run(context.Background(), "flood", syncrv1.HookPhase_HOOK_PHASE_BEFORE, nil)
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED || !run.GetOutputTruncated() {
		t.Fatalf("run status %v, truncated %v", run.GetStatus(), run.GetOutputTruncated())
	}
	out := run.GetOutput()
	if len(out) > link.MaxHookOutputBytes || len(out) < link.MaxHookOutputBytes-20 {
		t.Errorf("output is %d bytes, want just under %d", len(out), link.MaxHookOutputBytes)
	}
	if !strings.HasSuffix(out, "line 004999") || !strings.HasPrefix(out, "line 00") {
		t.Errorf("output = %q ... %q, want whole lines up to the last one", out[:20], out[len(out)-20:])
	}
}

// The environment of a hook is the supervisor's minimal one, the
// definition's variables and the caller's, which win: a definition cannot
// replace what the worker says about the job.
func TestRunEnvironment(t *testing.T) {
	t.Setenv("SYNCR_WORKER_SECRET", "the-worker-secret")
	t.Setenv("HOOKS_TEST_UNRELATED", "from the worker's environment")
	d := helper(t, "env", "env")
	d.Env["PGUSER"] = "backup"
	d.Env["SYNCR_JOB_ID"] = "set by the definition"
	s := newSet(t, d)
	run := s.Run(context.Background(), "env", syncrv1.HookPhase_HOOK_PHASE_BEFORE, map[string]string{
		"SYNCR_JOB_ID": "0195f0a4-3b7c-7d1e-9f00-000000000001", "SYNCR_HOOK_PHASE": "before",
	})
	if run.GetStatus() != syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED {
		t.Fatalf("run = %v", run)
	}
	env := strings.Split(run.GetOutput(), "\n")
	for _, want := range []string{"PGUSER=backup", "SYNCR_JOB_ID=0195f0a4-3b7c-7d1e-9f00-000000000001", "SYNCR_HOOK_PHASE=before", helperEnv + "=env"} {
		if !slices.Contains(env, want) {
			t.Errorf("the environment lacks %q:\n%s", want, run.GetOutput())
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "SYNCR_WORKER_SECRET=") || strings.HasPrefix(kv, "HOOKS_TEST_UNRELATED=") || strings.Contains(kv, "set by the definition") {
			t.Errorf("the environment holds %q", kv)
		}
	}
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(strings.ToUpper(kv), "PATH=") }) {
		t.Errorf("the environment has no PATH:\n%s", run.GetOutput())
	}
}

// fakeSupervisor returns a fixed result.
type fakeSupervisor struct {
	res  proc.Result
	err  error
	spec proc.Spec
}

func (f *fakeSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	f.spec = spec
	return f.res, f.err
}

func TestRunClassifiesSupervisorErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		res         proc.Result
		err         error
		wantStatus  syncrv1.HookStatus
		wantMessage string
		wantCode    int32
	}{
		"killed by a signal": {
			res: proc.Result{ExitCode: -1}, err: &proc.ExitError{Path: "x", ExitCode: -1, Err: errors.New("signal: killed")},
			wantStatus: syncrv1.HookStatus_HOOK_STATUS_FAILED, wantMessage: "was terminated by a signal", wantCode: -1,
		},
		// The command exited with code 0; something it started kept the
		// output open (a daemon it launched).
		"output held open": {
			res: proc.Result{ExitCode: 0}, err: fmt.Errorf("proc: x: %w", exec.ErrWaitDelay),
			wantStatus: syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED, wantCode: 0,
		},
		"anything else": {
			res: proc.Result{ExitCode: -1}, err: errors.New("boom"),
			wantStatus: syncrv1.HookStatus_HOOK_STATUS_FAILED, wantMessage: "could not be run: boom", wantCode: -1,
		},
		"a huge exit code": {
			res: proc.Result{ExitCode: 1 << 40}, err: &proc.ExitError{Path: "x", ExitCode: 1 << 40, Err: errors.New("exit")},
			wantStatus: syncrv1.HookStatus_HOOK_STATUS_FAILED, wantMessage: fmt.Sprintf("exited with code %d", 1<<40), wantCode: 1<<31 - 1,
		},
		"a long invalid message": {
			res: proc.Result{ExitCode: -1}, err: &proc.StartError{Path: "x", Err: errors.New(strings.Repeat("é", 400) + "\xff")},
			wantStatus: syncrv1.HookStatus_HOOK_STATUS_FAILED, wantCode: -1,
		},
	}
	for name, tt := range tests {
		sup := &fakeSupervisor{res: tt.res, err: tt.err}
		s, err := New(Config{Supervisor: sup, Definitions: []Definition{{Name: "x", Command: []string{"prog", "a", "b"}, Timeout: time.Minute}}})
		if err != nil {
			t.Fatal(err)
		}
		run := s.Run(context.Background(), "x", syncrv1.HookPhase_HOOK_PHASE_AFTER, nil)
		if run.GetStatus() != tt.wantStatus || run.GetExitCode() != tt.wantCode {
			t.Errorf("%s: status %v, exit code %d; want %v, %d", name, run.GetStatus(), run.GetExitCode(), tt.wantStatus, tt.wantCode)
		}
		if tt.wantMessage != "" && run.GetMessage() != tt.wantMessage {
			t.Errorf("%s: message %q, want %q", name, run.GetMessage(), tt.wantMessage)
		}
		if !utf8.ValidString(run.GetMessage()) || len(run.GetMessage()) > link.MaxHookMessageBytes {
			t.Errorf("%s: the message is not valid UTF-8 or longer than %d bytes: %d bytes", name, link.MaxHookMessageBytes, len(run.GetMessage()))
		}
		if sup.spec.Path != "prog" || !slices.Equal(sup.spec.Args, []string{"a", "b"}) || sup.spec.GracePeriod != DefaultGracePeriod {
			t.Errorf("%s: spec = %q %q, grace %s", name, sup.spec.Path, sup.spec.Args, sup.spec.GracePeriod)
		}
	}
}

func TestUnusable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Supervisor: proc.New(proc.Options{}), Definitions: []Definition{
		{Name: "fine", Command: []string{self}, Timeout: time.Second},
		{Name: "missing", Command: []string{filepath.Join(dir, "nope")}, Timeout: time.Second},
		{Name: "a-directory", Command: []string{dir}, Timeout: time.Second},
		{Name: "not-in-path", Command: []string{"syncr-no-such-program-9f2c"}, Timeout: time.Second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := s.Unusable()
	if len(got) != 3 || got["missing"] == nil || got["a-directory"] == nil || got["not-in-path"] == nil {
		t.Errorf("Unusable() = %v, want the three that cannot be started", got)
	}
}
