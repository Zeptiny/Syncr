//go:build integration

package system

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// hookScript is the operator's script behind the test worker's hooks: it
// notes what the worker told it, and plays a database dump written into
// the folder a job copies and removed again afterwards.
const hookScript = `#!/bin/sh
set -eu
echo "$1 $SYNCR_HOOK_NAME $SYNCR_HOOK_PHASE $SYNCR_JOB_KIND $SYNCR_JOB_STATUS $SYNCR_JOB_ERROR_CODE" >> "$MARKS"
case "$1" in
  dump) echo "dump of the database" > "$SRC/db.dump"; echo "wrote db.dump" ;;
  clean) rm -f "$SRC/db.dump"; echo "removed db.dump" ;;
  broken) echo "the database is down" >&2; exit 7 ;;
esac
`

// withHooks returns def naming hooks.
func withHooks(def *syncrv1.JobDefinition, h *syncrv1.JobHooks) *syncrv1.JobDefinition {
	def.Hooks = h
	return def
}

// hookRunLines renders a job's hook runs one per line, "name phase status
// exit", for comparison.
func hookRunLines(j *syncrv1.Job) string {
	var b strings.Builder
	for _, r := range j.GetHookRuns() {
		phase := strings.ToLower(strings.TrimPrefix(r.GetPhase().String(), "HOOK_PHASE_"))
		status := strings.ToLower(strings.TrimPrefix(r.GetStatus().String(), "HOOK_STATUS_"))
		fmt.Fprintf(&b, "%s %s %s %d\n", r.GetName(), phase, status, r.GetExitCode())
	}
	return b.String()
}

// TestSystemHooks: hooks written into worker.yaml are reported to the
// controller, offered by the API, and run by the real worker around a real
// copy: a dump written before the job is in the copy, and gone from the
// source afterwards.
func TestSystemHooks(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)

	src := filepath.Join(w.root, "src")
	writeRandom(t, filepath.Join(src, "a.bin"), 64<<10)
	script, marks := filepath.Join(w.dir, "hook.sh"), filepath.Join(w.dir, "marks")
	if err := os.WriteFile(script, []byte(hookScript), 0o700); err != nil {
		t.Fatal(err)
	}
	hook := func(name, description, arg string) string {
		return fmt.Sprintf("  %s:\n    description: %q\n    command: [\"/bin/sh\", %q, %q]\n    timeout: 30s\n    env:\n      SRC: %q\n      MARKS: %q\n",
			name, description, script, arg, src, marks)
	}
	hooksYAML := "\nhooks:\n" +
		hook("dump-db", "Dump the database into the source folder", "dump") +
		hook("remove-dump", "Remove the dump again", "clean") +
		hook("broken", "", "broken")
	f, err := os.OpenFile(w.config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(hooksYAML); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	w.start()

	// The worker reports its hooks: names, descriptions and timeouts, not
	// the commands.
	info := c.waitOnline(w.id).GetInfo()
	var names []string
	for _, h := range info.GetHooks() {
		names = append(names, h.GetName())
		if h.GetTimeout().AsDuration() != 30*time.Second {
			t.Errorf("hook %v", h)
		}
	}
	if !slices.Equal(names, []string{"broken", "dump-db", "remove-dump"}) || info.GetHooks()[1].GetDescription() != "Dump the database into the source folder" {
		t.Fatalf("the worker's hooks = %v", info.GetHooks())
	}
	if text := fmt.Sprint(info); strings.Contains(text, "hook.sh") || strings.Contains(text, marks) {
		t.Errorf("the worker's info names a hook's command or environment: %s", text)
	}

	// A job with hooks around it.
	dst := filepath.Join(w.root, "dst")
	j, _ := c.watch(c.runJob(w.id, withHooks(localCopy(src, dst, 0), &syncrv1.JobHooks{
		Before: []string{"dump-db"}, OnFailure: []string{"broken"}, After: []string{"remove-dump"},
	})))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	if got, want := hookRunLines(j), "dump-db before succeeded 0\nremove-dump after succeeded 0\n"; got != want {
		t.Errorf("hook runs:\n%swant:\n%s", got, want)
	}
	if got := j.GetHookRuns()[0].GetOutput(); strings.TrimSpace(got) != "wrote db.dump" {
		t.Errorf("output of dump-db = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "db.dump")); err != nil || string(got) != "dump of the database\n" {
		t.Errorf("the copy's dump = %q, %v: the hook did not run before the copy", got, err)
	}
	if _, err := os.Stat(filepath.Join(src, "db.dump")); !os.IsNotExist(err) {
		t.Errorf("the dump is still in the source (%v): the hook after the job did not run", err)
	}
	if j.GetStats().GetFiles() != 2 {
		t.Errorf("stats = %v", j.GetStats())
	}

	// A hook that fails before the job: nothing is copied, the hooks that
	// clean up run, and the job says which hook failed.
	dst2 := filepath.Join(w.root, "dst2")
	j, _ = c.watch(c.runJob(w.id, withHooks(localCopy(src, dst2, 0), &syncrv1.JobHooks{
		Before: []string{"dump-db", "broken"}, After: []string{"remove-dump"},
	})))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_FAILED)
	if got, want := hookRunLines(j), "dump-db before succeeded 0\nbroken before failed 7\nremove-dump after succeeded 0\n"; got != want {
		t.Errorf("hook runs:\n%swant:\n%s", got, want)
	}
	if j.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED || !strings.Contains(j.GetError().GetMessage(), `"broken"`) {
		t.Errorf("error = %v", j.GetError())
	}
	if got := j.GetHookRuns()[1].GetOutput(); !strings.Contains(got, "the database is down") {
		t.Errorf("output of broken = %q", got)
	}
	if _, err := os.Stat(dst2); !os.IsNotExist(err) {
		t.Errorf("the copy ran although a hook before it failed (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(src, "db.dump")); !os.IsNotExist(err) {
		t.Errorf("the dump is still in the source (%v)", err)
	}

	// What the worker told the script, run by run.
	got, err := os.ReadFile(marks)
	if err != nil {
		t.Fatal(err)
	}
	want := "dump dump-db before rclone_transfer running \n" +
		"clean remove-dump after rclone_transfer succeeded \n" +
		"dump dump-db before rclone_transfer running \n" +
		"broken broken before rclone_transfer running \n" +
		"clean remove-dump after rclone_transfer failed hook_failed\n"
	if string(got) != want {
		t.Errorf("the script noted:\n%swant:\n%s", got, want)
	}

	// The API refuses a hook the worker does not define, naming the field.
	_, err = c.jobs.RunJob(t.Context(), connect.NewRequest(&syncrv1.RunJobRequest{
		WorkerId: w.id, Definition: withHooks(localCopy(src, dst, 0), &syncrv1.JobHooks{Before: []string{"dump-db"}, After: []string{"wipe-disk"}}),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(fmt.Sprint(violations(err)), "definition.hooks.after[0]") {
		t.Errorf("RunJob with an unknown hook: %v (violations %v)", err, violations(err))
	}

	if err := w.proc.stop(30 * time.Second); err != nil {
		t.Errorf("worker stop: %v", err)
	}
}

// violations returns the field violations of an API error.
func violations(err error) []string {
	var out []string
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return nil
	}
	for _, d := range ce.Details() {
		v, derr := d.Value()
		if derr != nil {
			continue
		}
		if bad, ok := v.(*syncrv1.ValidationError); ok {
			for _, fv := range bad.GetViolations() {
				out = append(out, fv.GetField()+": "+fv.GetMessage())
			}
		}
	}
	return out
}
