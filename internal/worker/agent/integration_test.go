//go:build integration

package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclone"
)

// Integration tests run the real rclone runner (SYNCR_TEST_RCLONE) through
// the agent.

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
