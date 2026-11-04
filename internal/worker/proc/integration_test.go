//go:build integration

package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Integration tests run the real tools through the supervisor, with the
// minimal environment, env-defined remotes and JSON output that the rclone
// and restic runners rely on. Point SYNCR_TEST_RCLONE / SYNCR_TEST_RESTIC at
// the binaries (make test-integration fetches the pinned tools).

func toolPath(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s not set", env)
	}
	return path
}

func writeTree(t *testing.T, root string, files map[string]int) int64 {
	t.Helper()
	var total int64
	for name, size := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		total += int64(size)
	}
	return total
}

// rcloneStats is the subset of rclone's --use-json-log stats we check.
type rcloneStats struct {
	Stats *struct {
		Bytes          int64 `json:"bytes"`
		TotalBytes     int64 `json:"totalBytes"`
		Transfers      int64 `json:"transfers"`
		TotalTransfers int64 `json:"totalTransfers"`
	} `json:"stats"`
}

func rcloneSpec(rclone string, args ...string) Spec {
	return Spec{
		Path: rclone,
		Args: append(args, "--config", "", "--use-json-log", "--stats", "1s", "--stats-log-level", "NOTICE"),
		Env: map[string]string{
			"RCLONE_CONFIG_SRC_TYPE": "local",
			"RCLONE_CONFIG_DST_TYPE": "local",
		},
	}
}

func TestIntegrationRcloneCopyWithEnvRemotes(t *testing.T) {
	rclone := toolPath(t, "SYNCR_TEST_RCLONE")
	t.Setenv("SYNCR_WORKER_TOKEN", "must-not-leak") // ignored by the minimal env
	src, dst := t.TempDir(), t.TempDir()
	total := writeTree(t, src, map[string]int{"a.txt": 10, "sub/b.bin": 300_000})

	spec := rcloneSpec(rclone, "copy", "src:"+src, "dst:"+dst)
	var last rcloneStats
	var nonJSON []string
	spec.Stderr = func(l Line) {
		var s rcloneStats
		if err := json.Unmarshal(l.Data, &s); err != nil {
			nonJSON = append(nonJSON, string(l.Data))
			return
		}
		if s.Stats != nil {
			last = s
		}
	}

	res, err := New(Options{}).Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("rclone copy: %v (non-JSON output: %q)", err, nonJSON)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code %d", res.ExitCode)
	}
	if last.Stats == nil || last.Stats.Bytes != total || last.Stats.TotalBytes != total || last.Stats.Transfers != 2 {
		t.Errorf("final stats = %+v, want %d bytes in 2 transfers", last.Stats, total)
	}
	if len(nonJSON) > 0 {
		t.Errorf("rclone wrote non-JSON lines: %q", nonJSON)
	}
	if _, err := os.Stat(filepath.Join(dst, "sub", "b.bin")); err != nil {
		t.Errorf("destination file missing: %v", err)
	}
}

// TestIntegrationRcloneCancelStopsTheProcess: on Unix the supervisor sends
// SIGINT and rclone exits by itself; Windows has no equivalent for console
// programs, so there the process is killed. Either way Run returns promptly
// with a *CanceledError.
func TestIntegrationRcloneCancelStopsTheProcess(t *testing.T) {
	rclone := toolPath(t, "SYNCR_TEST_RCLONE")
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]int{"big.bin": 8 << 20})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spec := rcloneSpec(rclone, "copy", "src:"+src, "dst:"+dst, "--bwlimit", "256k")
	spec.Stderr = func(l Line) {
		var s rcloneStats
		if json.Unmarshal(l.Data, &s) == nil && s.Stats != nil {
			cancel() // first stats sample: the transfer is under way
		}
	}

	start := time.Now()
	res, err := New(Options{GracePeriod: 10 * time.Second}).Run(ctx, spec)
	if _, ok := errors.AsType[*CanceledError](err); !ok {
		t.Fatalf("Run() = %v, want *CanceledError", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("rclone took %v to stop; SIGINT should stop it promptly", elapsed)
	}
	if runtime.GOOS == "windows" {
		if res.ExitCode == 0 {
			t.Error("exit code 0 after a cancelled transfer; the process should have been killed")
		}
		return
	}
	if res.ExitCode == -1 {
		t.Error("rclone was killed instead of exiting on SIGINT")
	}
}

func TestIntegrationResticBackupToRcloneRepository(t *testing.T) {
	rclone := toolPath(t, "SYNCR_TEST_RCLONE")
	restic := toolPath(t, "SYNCR_TEST_RESTIC")
	data, repo := t.TempDir(), t.TempDir()
	writeTree(t, data, map[string]int{"docs/a.txt": 1000, "docs/b.txt": 2000})

	// restic reads each --option value as a CSV record (a pflag
	// StringSlice), then splits rclone.program with its own shell-like
	// splitter, in which a backslash outside quotes separates fields
	// (D:\a\rclone.exe would run "D:"). The restic runner encodes this in
	// general (rcloneOptions in runner/restic, which this package cannot
	// import); here the program is double-quoted for the splitter and the
	// whole option quoted for CSV.
	if strings.ContainsAny(rclone, "\"\r\n") || strings.HasSuffix(rclone, `\`) {
		t.Skipf("rclone path %q cannot be quoted for restic this simply", rclone)
	}
	programOption := `"rclone.program=""` + rclone + `"""`

	sup := New(Options{})
	resticRun := func(args ...string) []map[string]any {
		t.Helper()
		spec := Spec{
			Path: restic,
			Args: append([]string{
				"--repo", "rclone:repo:" + repo,
				"--option", programOption,
				"--option", "rclone.args=serve restic --stdio --config=",
				"--json",
			}, args...),
			Env: map[string]string{
				"RCLONE_CONFIG_REPO_TYPE": "local",
				"RESTIC_PASSWORD":         "integration-test",
				"RESTIC_CACHE_DIR":        t.TempDir(),
			},
		}
		var msgs []map[string]any
		var stderr []string
		spec.Stdout = func(l Line) {
			var m map[string]any
			if json.Unmarshal(l.Data, &m) == nil {
				msgs = append(msgs, m)
			}
		}
		spec.Stderr = func(l Line) { stderr = append(stderr, string(l.Data)) }
		if _, err := sup.Run(t.Context(), spec); err != nil {
			t.Fatalf("restic %v: %v (stderr: %q)", args, err, stderr)
		}
		return msgs
	}

	initMsgs := resticRun("init")
	if len(initMsgs) == 0 || initMsgs[len(initMsgs)-1]["message_type"] != "initialized" {
		t.Fatalf("restic init output = %v", initMsgs)
	}
	backup := resticRun("backup", data)
	summary := backup[len(backup)-1]
	if summary["message_type"] != "summary" || summary["snapshot_id"] == "" || summary["files_new"] != float64(2) {
		t.Fatalf("restic backup summary = %v", summary)
	}
}
