//go:build linux || windows

package proc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRunChildDiesWithWorker kills a process playing the worker while its
// supervised child runs, as a crash or SIGKILL would, and checks that the
// child does not outlive it.
func TestRunChildDiesWithWorker(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := exec.Command(self)
	worker.Env = append(os.Environ(), helperEnv+"=supervise")
	worker.Stderr = os.Stderr
	out, err := worker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Process.Kill() })

	var pid int
	sc := bufio.NewScanner(out)
	for pid == 0 && sc.Scan() {
		_, _ = fmt.Sscanf(sc.Text(), "pid=%d", &pid)
	}
	if pid == 0 {
		t.Fatal("child PID not received from the supervising process")
	}
	t.Cleanup(func() { killPID(pid) })
	if !processAlive(pid) {
		t.Fatalf("child %d is not running before the worker is killed", pid)
	}

	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running after its worker was killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
		_ = p.Release()
	}
}
