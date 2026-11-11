package upgrade

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

// fakeSupervisor prints canned output and records the spec it ran.
type fakeSupervisor struct {
	stdout, stderr []string
	err            error
	specs          []proc.Spec
}

func (s *fakeSupervisor) Run(_ context.Context, spec proc.Spec) (proc.Result, error) {
	s.specs = append(s.specs, spec)
	for _, l := range s.stdout {
		spec.Stdout(proc.Line{Data: []byte(l)})
	}
	for _, l := range s.stderr {
		spec.Stderr(proc.Line{Data: []byte(l)})
	}
	return proc.Result{}, s.err
}

func TestExecBinaryVersion(t *testing.T) {
	t.Parallel()
	sup := &fakeSupervisor{stdout: []string{"syncr-worker v2.1.0-rc.1 (commit 1a2b3c4, built 2026-11-20T10:00:00Z, go1.27.1) linux/amd64"}}
	v, err := NewExecBinary(sup).Version(t.Context(), "/opt/syncr/.syncr-worker-new-1")
	if err != nil || v != "v2.1.0-rc.1" {
		t.Fatalf("Version() = %q, %v", v, err)
	}
	if s := sup.specs[0]; s.Path != "/opt/syncr/.syncr-worker-new-1" || !slices.Equal(s.Args, []string{"version"}) {
		t.Errorf("ran %s %q", s.Path, s.Args)
	}

	for _, out := range [][]string{nil, {"rclone v1.75.2"}, {"syncr-worker"}} {
		if _, err := NewExecBinary(&fakeSupervisor{stdout: out}).Version(t.Context(), "/x"); err == nil {
			t.Errorf("Version() with output %q succeeded", out)
		}
	}
	failing := &fakeSupervisor{stderr: []string{"exec format error"}, err: errors.New("exit status 1")}
	if _, err := NewExecBinary(failing).Version(t.Context(), "/x"); err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Errorf("Version() error = %v, want the output quoted", err)
	}
}

func TestExecBinaryFetchTools(t *testing.T) {
	t.Parallel()
	sup := &fakeSupervisor{stdout: []string{
		"rclone  1.76.0  linux/amd64  installed   /var/lib/syncr/tools/rclone/1.76.0/linux-amd64/rclone",
		"restic  0.20.0  linux/amd64  up-to-date  /var/lib/syncr/tools/my tools/restic",
		"something else",
	}}
	pins, err := NewExecBinary(sup).FetchTools(t.Context(), "/x", "/var/lib/syncr/tools")
	if err != nil {
		t.Fatal(err)
	}
	if want := []tools.ToolVersion{{Tool: "rclone", Version: "1.76.0"}, {Tool: "restic", Version: "0.20.0"}}; !slices.Equal(pins, want) {
		t.Errorf("pins = %v, want %v", pins, want)
	}
	if args := sup.specs[0].Args; !slices.Equal(args, []string{"tools", "fetch", "--dir", "/var/lib/syncr/tools"}) {
		t.Errorf("args = %q", args)
	}
	if _, err := NewExecBinary(&fakeSupervisor{err: errors.New("exit status 1")}).FetchTools(t.Context(), "/x", "/t"); err == nil {
		t.Error("a failed tools fetch succeeded")
	}
}

func TestExecBinaryCheckTools(t *testing.T) {
	t.Parallel()
	sup := &fakeSupervisor{}
	err := NewExecBinary(sup).CheckTools(t.Context(), "/x", "/t", map[string]string{"rclone": "/opt/rclone", "restic": ""})
	if err != nil {
		t.Fatal(err)
	}
	s := sup.specs[0]
	if !slices.Equal(s.Args, []string{"tools", "status", "--dir", "/t"}) {
		t.Errorf("args = %q", s.Args)
	}
	if len(s.Env) != 1 || s.Env["SYNCR_RCLONE_PATH"] != "/opt/rclone" {
		t.Errorf("env = %v, want only the configured rclone path", s.Env)
	}

	failing := &fakeSupervisor{stdout: []string{"restic (pinned 0.20.0)", "  => none; run 'syncr-worker tools fetch'"}, err: errors.New("exit status 1")}
	err = NewExecBinary(failing).CheckTools(t.Context(), "/x", "/t", nil)
	if err == nil || !strings.Contains(err.Error(), "pinned tools") || !strings.Contains(err.Error(), "restic (pinned 0.20.0)") {
		t.Errorf("CheckTools() error = %v", err)
	}
}
