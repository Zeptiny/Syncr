// Package hooks runs a worker's hooks: the commands its operator defined in
// the worker's configuration (workerconfig.HookConfig) and that jobs name to
// have them run before or after their operation (syncr.v1.JobHooks,
// docs/hooks.md, docs/adr/0017-job-hooks.md).
//
// A Set holds the definitions and runs one hook at a time for a caller: the
// job agent, which decides which hooks run when and what their outcome
// means for the job (internal/worker/agent). The Set knows nothing of jobs
// beyond the environment variables the caller passes.
//
// # What a hook gets
//
// The command is started directly, without a shell, through the process
// supervisor that also runs rclone and restic (internal/worker/proc), so it
// has the same guarantees: a minimal environment (PATH, HOME, the locale and
// proxy settings of the worker, never the worker's own secrets or SYNCR_*
// settings), the definition's own variables on top, and then the caller's
// (the SYNCR_* variables that describe the job, which a definition cannot
// override); it is stopped when the caller's context ends or its timeout
// passes (a stop request, then a kill after the grace period, with every
// process it started); and it dies with the worker.
//
// Nothing a user of the controller types reaches the command line or the
// environment: a job contributes the hook's name, and the caller only
// passes values with a fixed character set (IDs, kind and status names).
//
// # Output
//
// Standard output and standard error are read line by line, and the last
// link.MaxHookOutputBytes of them are kept for the job's page. A hook is
// the operator's own program: the worker cannot know its secrets, so the
// output is reported as it is, made valid UTF-8. The Set logs nothing: its
// caller logs which hook ran and how it ended, never what it printed.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// internal/link and internal/worker/proc.
package hooks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// DefaultGracePeriod is how long a hook that was asked to stop (its
// timeout passed, or the job was cancelled) may take to exit before it is
// killed. It is shorter than the tools' grace period: a stopping worker has
// to fit a stopped hook and the hooks that clean up after it into its stop
// budget, and a shell script often does not act on the stop request at all
// while one of its commands runs.
const DefaultGracePeriod = 5 * time.Second

// Definition is one hook: a name and the command it stands for.
type Definition struct {
	// Name is the hook's name (link.ValidHookName).
	Name string
	// Description says what it does; it may be empty.
	Description string
	// Command is the program and its arguments.
	Command []string
	// Timeout bounds one run; it must be positive.
	Timeout time.Duration
	// Env is added to the command's environment.
	Env map[string]string
}

// Supervisor starts a command and waits for it. *proc.Supervisor implements
// it.
type Supervisor interface {
	Run(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

// Config configures a Set.
type Config struct {
	// Definitions are the hooks, in any order. Names must be unique.
	Definitions []Definition
	// Supervisor runs the commands. Required when there are definitions.
	Supervisor Supervisor
	// GracePeriod is how long a hook asked to stop may take to exit before
	// it is killed. Zero means DefaultGracePeriod.
	GracePeriod time.Duration
	// Now tells the time a run started. Nil means time.Now.
	Now func() time.Time
}

// Set is a worker's hooks. It is immutable and safe for concurrent use; the
// zero Set and a nil *Set define no hooks.
type Set struct {
	defs  map[string]Definition
	names []string // sorted
	sup   Supervisor
	grace time.Duration
	now   func() time.Time
}

// New returns the Set of cfg's definitions. It rejects what a worker
// configuration would not pass (config.Validate checks the same, with field
// names): an invalid or repeated name, a missing program, a timeout that is
// not positive, more than link.MaxWorkerHooks hooks.
func New(cfg Config) (*Set, error) {
	s := &Set{
		defs:  make(map[string]Definition, len(cfg.Definitions)),
		sup:   cfg.Supervisor,
		grace: cmp.Or(cfg.GracePeriod, DefaultGracePeriod),
		now:   cfg.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if len(cfg.Definitions) > 0 && cfg.Supervisor == nil {
		return nil, errors.New("hooks: a supervisor is required to run hooks")
	}
	if s.grace < 0 {
		return nil, fmt.Errorf("hooks: the grace period is %s", s.grace)
	}
	if len(cfg.Definitions) > link.MaxWorkerHooks {
		return nil, fmt.Errorf("hooks: %d hooks are defined, at most %d are allowed", len(cfg.Definitions), link.MaxWorkerHooks)
	}
	for _, d := range cfg.Definitions {
		switch {
		case !link.ValidHookName(d.Name):
			return nil, fmt.Errorf("hooks: %q is not a valid hook name", d.Name)
		case len(d.Command) == 0 || d.Command[0] == "":
			return nil, fmt.Errorf("hooks: hook %q has no command", d.Name)
		case d.Timeout <= 0:
			return nil, fmt.Errorf("hooks: hook %q has the timeout %s", d.Name, d.Timeout)
		}
		if _, dup := s.defs[d.Name]; dup {
			return nil, fmt.Errorf("hooks: hook %q is defined twice", d.Name)
		}
		d.Command = slices.Clone(d.Command)
		d.Env = maps.Clone(d.Env)
		s.defs[d.Name] = d
	}
	s.names = slices.Sorted(maps.Keys(s.defs))
	return s, nil
}

// Names returns the names of the hooks, sorted.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.names)
}

// Infos describes the hooks for the worker's Hello, sorted by name: what
// the controller may know of them. Commands and environments are left out.
func (s *Set) Infos() []*syncrv1.HookInfo {
	if s == nil {
		return nil
	}
	out := make([]*syncrv1.HookInfo, 0, len(s.names))
	for _, name := range s.names {
		d := s.defs[name]
		out = append(out, &syncrv1.HookInfo{Name: d.Name, Description: d.Description, Timeout: durationpb.New(d.Timeout)})
	}
	return out
}

// Check reports what makes h unusable on this worker, as a message for the
// job's owner: a list longer than link.MaxHooksPerPhase, a name listed
// twice in one list, and every name this worker does not define. Nil when
// every hook h names can be run (also when it names none).
func (s *Set) Check(h *syncrv1.JobHooks) error {
	var unknown []string
	for _, phase := range link.HookPhases() {
		names := link.HooksOf(h, phase)
		if len(names) > link.MaxHooksPerPhase {
			return fmt.Errorf("the job names %d %s hooks, at most %d are allowed", len(names), link.HookPhaseName(phase), link.MaxHooksPerPhase)
		}
		for i, name := range names {
			if slices.Contains(names[:i], name) {
				return fmt.Errorf("the job names the %s hook %q twice", link.HookPhaseName(phase), printable(name))
			}
			if !s.defined(name) && !slices.Contains(unknown, name) {
				unknown = append(unknown, name)
			}
		}
	}
	switch len(unknown) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("this worker defines no hook named %q: add it to the worker's configuration (hooks) or remove it from the job", printable(unknown[0]))
	}
	for i := range unknown {
		unknown[i] = fmt.Sprintf("%q", printable(unknown[i]))
	}
	return fmt.Errorf("this worker defines no hooks named %s: add them to the worker's configuration (hooks) or remove them from the job", strings.Join(unknown, ", "))
}

func (s *Set) defined(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.defs[name]
	return ok
}

// printable returns name as it may be shown: a name that is no hook name
// (it came over the link unchecked) is cut and made valid UTF-8, so an
// error message stays short and encodable.
func printable(name string) string {
	if link.ValidHookName(name) {
		return name
	}
	name = strings.ToValidUTF8(name, "�")
	if len(name) > link.MaxHookNameLen {
		cut := link.MaxHookNameLen
		for cut > 0 && name[cut]&0xC0 == 0x80 {
			cut-- // not inside a multi-byte character
		}
		name = name[:cut] + "..."
	}
	return name
}

// Unusable returns, for each hook whose program cannot be found now, why
// not, sorted by name: a bare name that is not in PATH, an absolute path
// that does not exist or is a directory. It is advice for the operator at
// startup, not a verdict: the program may live on a disk that is mounted
// later, and a run fails with the same explanation if it is still missing.
func (s *Set) Unusable() map[string]error {
	out := make(map[string]error)
	if s == nil {
		return out
	}
	for name, d := range s.defs {
		if err := lookProgram(d.Command[0]); err != nil {
			out[name] = err
		}
	}
	return out
}

// lookProgram checks that program can be started: found in PATH when it is
// a bare name, an existing file otherwise.
func lookProgram(program string) error {
	if filepath.Base(program) == program {
		_, err := exec.LookPath(program)
		return err
	}
	fi, err := os.Stat(program)
	switch {
	case err != nil:
		return err
	case fi.IsDir():
		return fmt.Errorf("%s is a directory", program)
	}
	return nil
}

// Stop causes of a run: errTimeout ends a hook that ran into its timeout.
var errTimeout = errors.New("the hook's timeout passed")

// Run runs the hook name once for phase and returns how it went; it never
// returns nil. env is added to the command's environment, over the
// definition's own variables (so the SYNCR_* variables the caller sets
// cannot be overridden by a definition).
//
// The run ends when the command exits, when the hook's timeout passes
// (TIMED_OUT) or when ctx is done (CANCELLED, with the context's cause as
// the message); in the last two cases the command is asked to stop and
// killed after the grace period. A hook this Set does not define, or whose
// command cannot be started, is a FAILED run that never started (exit code
// -1).
func (s *Set) Run(ctx context.Context, name string, phase syncrv1.HookPhase, env map[string]string) *syncrv1.HookRun {
	run := &syncrv1.HookRun{Name: name, Phase: phase, ExitCode: -1}
	var d Definition
	var ok bool
	if s != nil {
		d, ok = s.defs[name]
	}
	if !ok {
		run.Name = printable(name)
		run.Status = syncrv1.HookStatus_HOOK_STATUS_FAILED
		run.StartedAt = timestamppb.Now()
		run.Duration = durationpb.New(0)
		run.Message = "this worker defines no hook of that name"
		return run
	}

	out := newTail(link.MaxHookOutputBytes)
	spec := proc.Spec{
		Path:        d.Command[0],
		Args:        d.Command[1:],
		Env:         mergeEnv(d.Env, env),
		Stdout:      out.add,
		Stderr:      out.add,
		GracePeriod: s.grace,
	}
	tctx, cancel := context.WithTimeoutCause(ctx, d.Timeout, errTimeout)
	defer cancel()

	started := s.now()
	run.StartedAt = timestamppb.New(started)
	res, err := s.sup.Run(tctx, spec)
	run.Duration = durationpb.New(max(res.Duration, 0))
	run.ExitCode = int32(max(min(res.ExitCode, 1<<31-1), -1))
	run.Output, run.OutputTruncated = out.text()
	run.Status, run.Message = classify(ctx, d, err)
	run.Message = clipMessage(run.Message)
	return run
}

// classify turns the supervisor's error into a run's status and message.
// ctx is the caller's context (not the one with the hook's timeout).
func classify(ctx context.Context, d Definition, err error) (syncrv1.HookStatus, string) {
	if err == nil {
		return syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED, ""
	}
	if _, ok := errors.AsType[*proc.CanceledError](err); ok {
		// The caller's context first: a job cancelled just as the timeout
		// passed was cancelled.
		if ctx.Err() != nil {
			return syncrv1.HookStatus_HOOK_STATUS_CANCELLED, "stopped: " + context.Cause(ctx).Error()
		}
		return syncrv1.HookStatus_HOOK_STATUS_TIMED_OUT, fmt.Sprintf("timed out after %s", d.Timeout)
	}
	if e, ok := errors.AsType[*proc.StartError](err); ok {
		if ctx.Err() != nil {
			return syncrv1.HookStatus_HOOK_STATUS_CANCELLED, "stopped before it started: " + context.Cause(ctx).Error()
		}
		return syncrv1.HookStatus_HOOK_STATUS_FAILED, "could not be started: " + e.Err.Error()
	}
	if e, ok := errors.AsType[*proc.ExitError](err); ok {
		if e.ExitCode < 0 {
			return syncrv1.HookStatus_HOOK_STATUS_FAILED, "was terminated by a signal"
		}
		return syncrv1.HookStatus_HOOK_STATUS_FAILED, fmt.Sprintf("exited with code %d", e.ExitCode)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command itself exited with code 0; a process it left behind
		// kept its output open past the grace period. The hook did what it
		// was asked to do.
		return syncrv1.HookStatus_HOOK_STATUS_SUCCEEDED, ""
	}
	return syncrv1.HookStatus_HOOK_STATUS_FAILED, "could not be run: " + err.Error()
}

// clipMessage bounds a run's message to link.MaxHookMessageBytes of valid
// UTF-8.
func clipMessage(msg string) string {
	msg = strings.ToValidUTF8(msg, "�")
	if len(msg) <= link.MaxHookMessageBytes {
		return msg
	}
	cut := link.MaxHookMessageBytes - len("...")
	for cut > 0 && msg[cut]&0xC0 == 0x80 {
		cut--
	}
	return msg[:cut] + "..."
}

// mergeEnv returns own overlaid with call.
func mergeEnv(own, call map[string]string) map[string]string {
	out := make(map[string]string, len(own)+len(call))
	maps.Copy(out, own)
	maps.Copy(out, call)
	return out
}
