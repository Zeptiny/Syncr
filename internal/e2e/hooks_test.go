package e2e

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/hooks"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// The test binary doubles as the command of the workers' hooks: when
// hookHelperEnv is set, TestMain runs the helper instead of the tests. The
// variable reaches the command through the hook's own environment, as an
// operator's variables do.
const hookHelperEnv = "E2E_HOOK_HELPER"

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(hookHelperEnv); ok {
		os.Exit(hookHelper(mode))
	}
	os.Exit(m.Run())
}

// hookHelper is a hook's command: it prints what the worker told it about
// the job and exits with 0, or with 3 in mode "fail".
func hookHelper(mode string) int {
	fmt.Printf("%s %s %s %s\n", os.Getenv("SYNCR_HOOK_NAME"), os.Getenv("SYNCR_HOOK_PHASE"),
		os.Getenv("SYNCR_JOB_STATUS"), os.Getenv("SYNCR_JOB_ERROR_CODE"))
	if mode == "fail" {
		// To the same stream as the line above: the order of two streams
		// in the kept output is not defined.
		fmt.Println("the database is not there")
		return 3
	}
	return 0
}

// hookSet returns a worker's hooks, each running this test binary: the
// names in failing exit with code 3, the others with 0.
func hookSet(t *testing.T, names []string, failing ...string) *hooks.Set {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	defs := make([]hooks.Definition, 0, len(names))
	for _, name := range names {
		env := map[string]string{hookHelperEnv: "ok"}
		if slices.Contains(failing, name) {
			env[hookHelperEnv] = "fail"
		}
		if dir, ok := os.LookupEnv("GOCOVERDIR"); ok {
			env["GOCOVERDIR"] = dir // keep coverage builds quiet
		}
		defs = append(defs, hooks.Definition{
			Name: name, Description: "Test hook " + name, Command: []string{self}, Timeout: 30 * time.Second, Env: env,
		})
	}
	set, err := hooks.New(hooks.Config{Definitions: defs, Supervisor: proc.New(proc.Options{}), GracePeriod: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// hookedDefinition is testDefinition naming hooks.
func hookedDefinition(h *syncrv1.JobHooks) *syncrv1.JobDefinition {
	def := testDefinition()
	def.Hooks = h
	return def
}

// hookLines renders a job's hook runs one per line, "name phase status
// exit: output", for comparison.
func hookLines(runs []jobs.HookRun) string {
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, "%s %s %s %d: %s\n", r.Name, r.Phase, r.Status, r.ExitCode, strings.Join(strings.Fields(r.Output), " "))
	}
	return b.String()
}

// TestActiveJobHooks runs jobs with hooks end to end: the worker reports
// the hooks it defines, the controller sends a job's hook names, the worker
// runs the commands around the operation, and the controller stores how
// each went.
func TestActiveJobHooks(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("nas")
	rn := newFakeRunner()
	o := upToDate(nil)
	o.hooks = hookSet(t, []string{"stop-app", "dump-db", "notify", "start-app"}, "dump-db")
	startActiveWith(t, rn, o, activeOptions(c, id, secret))

	w := c.waitWorker(id, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })
	if got := w.Info.HookNames(); !slices.Equal(got, []string{"dump-db", "notify", "start-app", "stop-app"}) {
		t.Fatalf("the worker's hooks = %v", got)
	}
	for _, h := range w.Info.Hooks {
		if h.Description != "Test hook "+h.Name || h.Timeout != 30*time.Second {
			t.Errorf("hook %+v", h)
		}
	}

	t.Run("around a job that succeeds", func(t *testing.T) {
		job := c.run(id, hookedDefinition(&syncrv1.JobHooks{
			Before: []string{"stop-app"}, OnSuccess: []string{"notify"}, OnFailure: []string{"dump-db"}, After: []string{"start-app"},
		}))
		j := c.waitStatus(job, domain.JobSucceeded)
		want := "stop-app before succeeded 0: stop-app before running\n" +
			"notify on_success succeeded 0: notify on_success succeeded\n" +
			"start-app after succeeded 0: start-app after succeeded\n"
		if got := hookLines(j.HookRuns); got != want {
			t.Errorf("hook runs:\n%swant:\n%s", got, want)
		}
		if rn.runCount(job) != 1 || j.Stats.Bytes != 100 {
			t.Errorf("runs %d, stats %+v", rn.runCount(job), j.Stats)
		}
		// The worker got the names and nothing else of the hooks.
		if h := rn.spec(job).GetHooks(); !slices.Equal(h.GetBefore(), []string{"stop-app"}) || !slices.Equal(h.GetAfter(), []string{"start-app"}) {
			t.Errorf("spec hooks = %v", h)
		}
		for _, r := range j.HookRuns {
			if r.StartedAt.IsZero() || r.Duration <= 0 || r.Message != "" || r.OutputTruncated {
				t.Errorf("hook run %+v", r)
			}
		}
	})

	t.Run("a failing hook before the job skips its operation", func(t *testing.T) {
		job := c.run(id, hookedDefinition(&syncrv1.JobHooks{
			Before: []string{"stop-app", "dump-db", "notify"}, OnSuccess: []string{"notify"}, OnFailure: []string{"notify"}, After: []string{"start-app"},
		}))
		j := c.waitStatus(job, domain.JobFailed)
		want := "stop-app before succeeded 0: stop-app before running\n" +
			"dump-db before failed 3: dump-db before running the database is not there\n" +
			"notify on_failure succeeded 0: notify on_failure failed hook_failed\n" +
			"start-app after succeeded 0: start-app after failed hook_failed\n"
		if got := hookLines(j.HookRuns); got != want {
			t.Errorf("hook runs:\n%swant:\n%s", got, want)
		}
		if j.Error.Code != jobs.CodeHookFailed || !strings.Contains(j.Error.Message, `"dump-db"`) || rn.runCount(job) != 0 {
			t.Errorf("error %+v, runs %d", j.Error, rn.runCount(job))
		}
	})

	t.Run("a hook the worker does not define is never sent", func(t *testing.T) {
		job := c.run(id, hookedDefinition(&syncrv1.JobHooks{Before: []string{"stop-app", "wipe-disk"}}))
		j := c.waitStatus(job, domain.JobFailed)
		if j.Error.Code != jobs.CodeInvalidSpec || !strings.Contains(j.Error.Message, "wipe-disk") || rn.runCount(job) != 0 || len(j.HookRuns) != 0 {
			t.Errorf("error %+v, runs %d, hook runs %v", j.Error, rn.runCount(job), j.HookRuns)
		}
	})
}

// TestPassiveJobHooks: the same over the passive link, where the controller
// calls the worker.
func TestPassiveJobHooks(t *testing.T) {
	t.Parallel()
	c := newController(t)
	secret := pairingSecret()
	certFile, keyFile, _ := selfSigned(t)
	rn := newFakeRunner()
	o := upToDate(nil)
	o.hooks = hookSet(t, []string{"stop-app", "start-app"}, "start-app")
	pw := startPassiveOnWith(t, rn, o, certFile, keyFile, secret, listen(t))
	_, ws := c.svc()
	reg, err := ws.RegisterPassive(context.Background(), workers.PassiveParams{
		Name: "server", URL: pw.url, CertFingerprint: pw.fingerprint, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := reg.ID
	c.wakeSupervisor()

	c.waitWorker(id, "hello", func(w workers.Worker) bool { return len(w.Info.Hooks) == 2 })
	job := c.run(id, hookedDefinition(&syncrv1.JobHooks{Before: []string{"stop-app"}, After: []string{"start-app"}}))
	j := c.waitStatus(job, domain.JobFailed)
	want := "stop-app before succeeded 0: stop-app before running\n" +
		"start-app after failed 3: start-app after succeeded the database is not there\n"
	if got := hookLines(j.HookRuns); got != want {
		t.Errorf("hook runs:\n%swant:\n%s", got, want)
	}
	// The operation ran and its result is kept, although the hook that
	// cleans up failed the job.
	if j.Error.Code != jobs.CodeHookFailed || rn.runCount(job) != 1 || j.Stats.Bytes != 100 {
		t.Errorf("error %+v, runs %d, stats %+v", j.Error, rn.runCount(job), j.Stats)
	}
}

// TestActiveJobHooksNeedAnUpToDateWorker: a job with hooks for a worker
// that does not report the hooks feature fails with worker_outdated and is
// never sent.
func TestActiveJobHooksNeedAnUpToDateWorker(t *testing.T) {
	t.Parallel()
	c := newController(t)
	id, secret := c.enrollActive("old")
	rn := newFakeRunner()
	features := slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureJobHooks })
	startActiveWith(t, rn, workerOptions{features: features}, activeOptions(c, id, secret))
	c.waitWorker(id, "hello", func(w workers.Worker) bool { return w.Info.Hostname == "e2e" })

	job := c.run(id, hookedDefinition(&syncrv1.JobHooks{Before: []string{"stop-app"}}))
	j := c.waitStatus(job, domain.JobFailed)
	if j.Error.Code != jobs.CodeWorkerOutdated || !strings.Contains(j.Error.Message, "hooks") || rn.runCount(job) != 0 {
		t.Errorf("error %+v, runs %d", j.Error, rn.runCount(job))
	}
	// Without hooks the same worker runs the job.
	c.waitStatus(c.run(id, testDefinition()), domain.JobSucceeded)
}
