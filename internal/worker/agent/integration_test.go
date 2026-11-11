//go:build integration

package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclone"
	"github.com/Zeptiny/Syncr/internal/worker/runner/restic"
)

// Integration tests run the real rclone runner (SYNCR_TEST_RCLONE) and the
// real restic runners (SYNCR_TEST_RESTIC) through the agent.

// rcloneAgent builds an agent whose registry holds the real rclone runner,
// allowed to touch root.
func rcloneAgent(t *testing.T, root string) *Agent {
	t.Helper()
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	policy, err := localfs.NewPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := rclone.New(rclone.Config{
		Binary:     bin,
		Supervisor: proc.New(proc.Options{GracePeriod: 5 * time.Second}),
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: filepath.Join(t.TempDir(), "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := runner.NewRegistry()
	if err := reg.Register(rn); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Runners: reg, Identity: Identity{LocalRoots: []string{root}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return a
}

// waitResult waits (in real time) until job's result is in the outbox.
func waitResult(t *testing.T, a *Agent, job string, timeout time.Duration) *syncrv1.JobResult {
	t.Helper()
	deadline := time.After(timeout)
	for {
		for _, e := range events(a) {
			if r := e.GetJobResult(); r.GetJobId() == job {
				return r
			}
		}
		select {
		case <-a.Outbox().Notify():
		case <-deadline:
			t.Fatalf("no result for %s after %v; events: %s", job, timeout, describe(events(a)))
		}
	}
}

func TestIntegrationRcloneCopyThroughAgent(t *testing.T) {
	root := t.TempDir()
	a := rcloneAgent(t, root)
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	files := map[string]string{"a.txt": "alpha\n", filepath.Join("sub", "b.txt"): "bravo\n", filepath.Join("sub", "c.bin"): strings.Repeat("c", 1<<20)}
	for name, content := range files {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	spec := &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: src},
		Destination: &syncrv1.Location{Path: dst},
	}}}
	cmd := &syncrv1.Command{Id: "cmd-1", Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: "copy-1", Spec: spec}}}
	if acks := a.HandleCommands(t.Context(), []*syncrv1.Command{cmd, cmd}); len(acks) != 2 {
		t.Fatalf("acks = %v", acks)
	}
	r := waitResult(t, a, "copy-1", time.Minute)
	if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED || r.GetExitCode() != 0 || r.GetError() != nil {
		t.Fatalf("result = %v", r)
	}
	if r.GetStats().GetFiles() != int64(len(files)) || r.GetStats().GetBytes() < 1<<20 {
		t.Errorf("stats = %v", r.GetStats())
	}
	if r.GetStartedAt() == nil || r.GetFinishedAt().AsTime().Before(r.GetStartedAt().AsTime()) {
		t.Errorf("times: started %v finished %v", r.GetStartedAt(), r.GetFinishedAt())
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || !bytes.Equal(got, []byte(content)) {
			t.Errorf("%s: copied %d bytes, err %v", name, len(got), err)
		}
	}

	// Event sequence: hello, start, at least one (final) progress sample,
	// exactly one result.
	evs, _ := a.Outbox().Pending(0, 0)
	kinds := make([]string, 0, len(evs))
	for _, e := range evs {
		switch {
		case e.GetHello() != nil:
			kinds = append(kinds, "hello")
		case e.GetJobStarted() != nil:
			kinds = append(kinds, "start")
		case e.GetProgress() != nil:
			if len(kinds) == 0 || (kinds[len(kinds)-1] != "progress" && kinds[len(kinds)-1] != "start") {
				t.Errorf("progress out of place: %v", kinds)
			}
			kinds = append(kinds, "progress")
		case e.GetJobResult() != nil:
			kinds = append(kinds, "result")
		}
	}
	got := strings.Join(kinds, " ")
	if !strings.HasPrefix(got, "hello start progress") || !strings.HasSuffix(got, "progress result") || strings.Count(got, "result") != 1 {
		t.Errorf("event kinds = %s", got)
	}
	if hello := evs[0].GetHello(); len(hello.GetJobKinds()) != 1 || hello.GetJobKinds()[0] != "rclone_transfer" {
		t.Errorf("hello = %v", hello)
	}
	if st := a.State(); len(st.GetJobs()) != 0 {
		t.Errorf("state after finish = %v", st)
	}
}

func TestIntegrationRcloneCancelThroughAgent(t *testing.T) {
	root := t.TempDir()
	a := rcloneAgent(t, root)
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), bytes.Repeat([]byte{7}, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	limit := int64(256 << 10) // 256 KiB/s: the copy would take ~30s
	spec := &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.Location{Path: src},
		Destination: &syncrv1.Location{Path: filepath.Join(root, "dst")},
		Options:     &syncrv1.RcloneTransferOptions{BandwidthLimitBytesPerSecond: &limit},
	}}}
	a.HandleCommands(t.Context(), []*syncrv1.Command{{Id: "c1", Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: "slow", Spec: spec}}}})

	// Wait for the first progress sample, then cancel.
	deadline := time.After(30 * time.Second)
	for !strings.Contains(describe(events(a)), "progress(slow") {
		select {
		case <-a.Outbox().Notify():
		case <-deadline:
			t.Fatalf("no progress: %s", describe(events(a)))
		}
	}
	start := time.Now()
	a.HandleCommands(t.Context(), []*syncrv1.Command{{Id: "c2", Command: &syncrv1.Command_CancelJob{CancelJob: &syncrv1.CancelJob{JobId: "slow"}}}})
	r := waitResult(t, a, "slow", 20*time.Second)
	if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED || r.GetError().GetMessage() != ErrCancelled.Error() {
		t.Errorf("result = %v", r)
	}
	t.Logf("cancelled after %v", time.Since(start))
}

// slowUploads adds restic flags to every run, so a small backup lasts long
// enough to be stopped while it uploads.
type slowUploads struct {
	restic.Supervisor
	flags []string
}

func (s slowUploads) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	spec.Args = append(append([]string{}, s.flags...), spec.Args...)
	return s.Supervisor.Run(ctx, spec)
}

// localRemote defines every remote as a local-filesystem rclone remote (the
// worker's builder refuses the local backend; restic's rclone backend over
// it is what matters here).
type localRemote struct{}

func (localRemote) Env(remotes []*syncrv1.RemoteDefinition) (map[string]string, error) {
	env := map[string]string{}
	for _, r := range remotes {
		env["RCLONE_CONFIG_"+strings.ToUpper(r.GetAlias())+"_TYPE"] = "local"
	}
	return env, nil
}

// rootPolicy allows every path below root.
type rootPolicy struct{ root string }

func (p rootPolicy) Resolve(path string) (string, error) {
	pol, err := localfs.NewPolicy([]string{p.root})
	if err != nil {
		return "", err
	}
	return pol.Resolve(path)
}

// liveProcessesWith returns the PIDs of live processes whose command line
// contains s (Linux).
func liveProcessesWith(t *testing.T, s string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, []byte(s)) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if i := bytes.LastIndexByte(stat, ')'); err == nil && i > 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X') {
			continue // exited, waiting to be reaped
		}
		pids = append(pids, pid)
	}
	return pids
}

// TestIntegrationResticTimeLimitThroughAgent runs a real restic backup to a
// repository behind restic's rclone backend (restic starts "rclone serve
// restic") and lets the job's time limit stop it while it uploads. The job
// fails with TIME_LIMIT and no process of the job is left.
//
//   - With the usual grace period restic stops on its own and releases its
//     repository lock on the way out: the stop request reaches restic
//     alone, while its rclone still serves it.
//   - With a grace period too short for that, restic is killed; nothing it
//     started outlives it either. (The lock restic left is stale; restic's
//     own rules deal with those.) Here its rclone also ends by itself once
//     restic's pipes close, so the proc package's tree tests, whose
//     grandchild ignores everything but SIGKILL, are what proves the kill.
func TestIntegrationResticTimeLimitThroughAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("looks for leftover processes in /proc")
	}
	resticBin, rcloneBin := os.Getenv("SYNCR_TEST_RESTIC"), os.Getenv("SYNCR_TEST_RCLONE")
	if resticBin == "" || rcloneBin == "" {
		t.Skip("SYNCR_TEST_RESTIC or SYNCR_TEST_RCLONE not set")
	}
	for _, tt := range []struct {
		name   string
		grace  time.Duration
		noLock bool
	}{
		{name: "graceful stop", grace: 15 * time.Second, noLock: true},
		{name: "killed after the grace period", grace: time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			timeLimitedBackup(t, resticBin, rcloneBin, tt.grace, tt.noLock)
		})
	}
}

func timeLimitedBackup(t *testing.T, resticBin, rcloneBin string, grace time.Duration, wantNoLock bool) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "roots")
	// A unique name to find the job's processes by their arguments.
	repoDir := filepath.Join(base, "repo-time-limit-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	src := filepath.Join(root, "data")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4_000_000) // random, so it does not compress: ~8s at 500 KiB/s
	_, _ = rand.Read(data)
	if err := os.WriteFile(filepath.Join(src, "big.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	runners, err := restic.New(restic.Config{
		ResticPath: resticBin, RclonePath: rcloneBin,
		Supervisor:  slowUploads{Supervisor: proc.New(proc.Options{GracePeriod: grace}), flags: []string{"--limit-upload=500"}},
		Remotes:     localRemote{},
		Paths:       rootPolicy{root: root},
		ScratchRoot: filepath.Join(base, "scratch"),
		CacheDir:    filepath.Join(base, "cache"),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := runner.NewRegistry()
	if err := reg.Register(runners...); err != nil {
		t.Fatal(err)
	}
	var timers fakeTimers
	a, err := New(Config{Runners: reg, AfterFunc: timers.afterFunc})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "repo", Path: repoDir}, Password: "integration-test-password"}
	remotes := []*syncrv1.RemoteDefinition{{Alias: "repo", BackendType: "local"}}
	start := func(cmdID, jobID string, spec *syncrv1.JobSpec) {
		spec.Remotes = remotes
		a.HandleCommands(t.Context(), []*syncrv1.Command{{Id: cmdID, Command: &syncrv1.Command_StartJob{StartJob: &syncrv1.StartJob{JobId: jobID, Spec: spec}}}})
	}
	start("c1", "init-1", &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: repo}}})
	if r := waitResult(t, a, "init-1", 2*time.Minute); r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_SUCCEEDED {
		t.Fatalf("init: %v", r)
	}

	backup := &syncrv1.JobSpec{
		MaxRuntime: durationpb.New(time.Hour),
		Operation:  &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{Repository: repo, Paths: []string{src}, Host: "syncr-it"}},
	}
	start("c2", "backup-1", backup)
	deadline := time.After(time.Minute)
	for !strings.Contains(describe(events(a)), "progress(backup-1") {
		select {
		case <-a.Outbox().Notify():
		case <-deadline:
			t.Fatalf("no progress: %s", describe(events(a)))
		}
	}
	// The backup uploads: restic and the rclone it started both run.
	if pids := liveProcessesWith(t, repoDir); len(pids) < 2 {
		t.Fatalf("processes of the job: %v, want restic and its rclone", pids)
	}
	timers.fire(timers.only(t)) // the hour has passed

	r := waitResult(t, a, "backup-1", 30*time.Second)
	if r.GetStatus() != syncrv1.JobResultStatus_JOB_RESULT_STATUS_FAILED ||
		r.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT ||
		r.GetError().GetMessage() != "stopped after 1h0m0s: the job's time limit was reached" {
		t.Fatalf("result = %v", r)
	}
	if wantNoLock {
		if locks, err := os.ReadDir(filepath.Join(repoDir, "locks")); err != nil || len(locks) != 0 {
			t.Errorf("repository locks after the stop: %d entries, %v; want none", len(locks), err)
		}
	}
	// No process of the job left. The kill was sent before the result; the
	// kernel may take a moment to finish them.
	gone := time.After(10 * time.Second)
	for pids := liveProcessesWith(t, repoDir); len(pids) > 0; pids = liveProcessesWith(t, repoDir) {
		select {
		case <-gone:
			t.Fatalf("processes of the job still running: %v", pids)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
