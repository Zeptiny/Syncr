package service

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func decodeUTF16(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("missing UTF-16LE byte order mark: % x", b[:min(4, len(b))])
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 2; i+1 < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:]))
	}
	return string(utf16.Decode(units))
}

func windowsUserSpec() Spec {
	return NewSpec("windows", ScopeUser, `C:\Users\Alice Smith\bin\syncr-worker.exe`, `C:\Users\Alice Smith\AppData\Local\Syncr\worker.yaml`,
		`C:\Users\Alice Smith\AppData\Local\Syncr`, `C:\Users\Alice Smith\AppData\Local\Syncr\logs\worker.log`)
}

func TestTaskXML(t *testing.T) {
	t.Parallel()
	spec := windowsUserSpec()
	raw, err := taskXML(spec, `LAPTOP\Alice & Co`)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeUTF16(t, raw)
	if !strings.HasPrefix(text, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("header = %q", text[:60])
	}

	// The document must be well-formed and say what we mean.
	var doc taskDocument
	body := strings.TrimPrefix(text, `<?xml version="1.0" encoding="UTF-16"?>`)
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, text)
	}
	if doc.LogonTrigger.UserID != `LAPTOP\Alice & Co` || doc.Principal.UserID != `LAPTOP\Alice & Co` {
		t.Errorf("trigger/principal user = %q / %q: the logon trigger must name the user, or creating the task needs administrator rights",
			doc.LogonTrigger.UserID, doc.Principal.UserID)
	}
	if doc.Principal.RunLevel != "LeastPrivilege" || doc.Principal.LogonType != "InteractiveToken" {
		t.Errorf("principal = %+v", doc.Principal)
	}
	if doc.Actions.Exec.Command != spec.Executable {
		t.Errorf("command = %q", doc.Actions.Exec.Command)
	}
	wantArgs := `run --config "C:\Users\Alice Smith\AppData\Local\Syncr\worker.yaml" --log-file "C:\Users\Alice Smith\AppData\Local\Syncr\logs\worker.log" --hide-console`
	if doc.Actions.Exec.Arguments != wantArgs {
		t.Errorf("arguments = %q, want %q", doc.Actions.Exec.Arguments, wantArgs)
	}
	s := doc.Settings
	if s.ExecutionTimeLimit != "PT0S" || s.RestartOnFailure.Count != 999 || s.RestartOnFailure.Interval != "PT1M" ||
		s.DisallowStartIfOnBatteries || s.StopIfGoingOnBatteries || !s.AllowHardTerminate || !s.Enabled || s.MultipleInstancesPolicy != "IgnoreNew" {
		t.Errorf("settings = %+v", s)
	}
	if doc.Actions.Context != doc.Principal.ID {
		t.Errorf("action context %q != principal id %q", doc.Actions.Context, doc.Principal.ID)
	}
	if !strings.Contains(text, "Alice &amp; Co") {
		t.Error("special characters are not escaped")
	}
}

func TestWindowsQuote(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`plain`:              `plain`,
		``:                   `""`,
		`with space`:         `"with space"`,
		`C:\dir\file.yaml`:   `C:\dir\file.yaml`,
		`C:\my dir\`:         `"C:\my dir\\"`,
		`say "hi"`:           `"say \"hi\""`,
		`back\slash "q"`:     `"back\slash \"q\""`,
		`ends with\\ space `: `"ends with\\ space "`,
	}
	for in, want := range tests {
		if got := windowsQuote(in); got != want {
			t.Errorf("windowsQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

type taskHarness struct {
	tm     *taskManager
	runner *fakeRunner
	xml    string // task definition passed to /Create
}

func newTaskHarness(t *testing.T, reply func(name string, args []string) (string, int, error)) *taskHarness {
	t.Helper()
	h := &taskHarness{runner: &fakeRunner{}}
	h.runner.reply = func(name string, args []string) (string, int, error) {
		if i := slices.Index(args, "/XML"); i >= 0 {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				t.Errorf("the task definition is not readable while schtasks runs: %v", err)
			}
			h.xml = decodeUTF16(t, b)
		}
		if reply != nil {
			return reply(name, args)
		}
		return "", 0, nil
	}
	h.tm = &taskManager{runner: h.runner, user: func() (string, error) { return `PC\alice`, nil }, tempDir: t.TempDir}
	return h
}

func TestTaskInstallCommandLine(t *testing.T) {
	t.Parallel()
	h := newTaskHarness(t, nil)
	spec := windowsUserSpec()
	spec.LogFile = t.TempDir() + "/logs/worker.log"
	if err := h.tm.install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if len(h.runner.calls) != 1 || !strings.HasPrefix(h.runner.calls[0], "schtasks.exe /Create /TN syncr-worker /XML ") {
		t.Fatalf("calls = %q", h.runner.calls)
	}
	if strings.Contains(h.runner.calls[0], "/F") || strings.Contains(h.runner.calls[0], "/RU") {
		t.Errorf("create must not force or name a run-as user: %q", h.runner.calls[0])
	}
	if !strings.Contains(h.xml, `PC\alice`) {
		t.Errorf("task XML lacks the user:\n%s", h.xml)
	}
	if _, err := os.Stat(strings.TrimSuffix(spec.LogFile, "/worker.log")); err != nil {
		t.Errorf("log directory not created: %v", err)
	}
}

func TestTaskInstallFailure(t *testing.T) {
	t.Parallel()
	h := newTaskHarness(t, func(string, []string) (string, int, error) { return "ERROR: Access is denied.", 1, nil })
	err := h.tm.install(t.Context(), windowsUserSpec())
	if err == nil || !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestTaskControl(t *testing.T) {
	t.Parallel()
	spec := windowsUserSpec()
	h := newTaskHarness(t, nil)
	ctx := context.Background()
	for _, action := range []string{"start", "stop", "restart"} {
		if err := h.tm.control(ctx, spec, action); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.tm.uninstall(ctx, spec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schtasks.exe /Run /TN syncr-worker",
		"schtasks.exe /End /TN syncr-worker",
		"schtasks.exe /End /TN syncr-worker", // restart: stop ...
		"schtasks.exe /Run /TN syncr-worker", // ... then start
		"schtasks.exe /End /TN syncr-worker", // uninstall: stop ...
		"schtasks.exe /Delete /TN syncr-worker /F",
	}
	if !slices.Equal(h.runner.calls, want) {
		t.Errorf("calls =\n%q\nwant\n%q", h.runner.calls, want)
	}

	h = newTaskHarness(t, func(_ string, args []string) (string, int, error) {
		if slices.Contains(args, "/End") {
			return "ERROR: There are no running instances of the task.", 1, nil
		}
		return "", 0, nil
	})
	if err := h.tm.control(ctx, spec, "stop"); err == nil {
		t.Error("a failing /End was ignored")
	}
}

func TestTaskStatus(t *testing.T) {
	t.Parallel()
	spec := windowsUserSpec()
	for _, tt := range []struct {
		name    string
		queryOK bool
		state   string
		want    State
	}{
		{"missing", false, "", StateNotInstalled},
		{"running", true, "Running\r\n", StateRunning},
		{"ready", true, "Ready", StateStopped},
		{"disabled", true, "Disabled", StateStopped},
	} {
		h := newTaskHarness(t, func(name string, _ []string) (string, int, error) {
			if name == "schtasks.exe" {
				if tt.queryOK {
					return "", 0, nil
				}
				return "ERROR: The system cannot find the file specified.", 1, nil
			}
			return tt.state, 0, nil
		})
		got, err := h.tm.status(t.Context(), spec)
		if err != nil || got != tt.want {
			t.Errorf("%s: status = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

func TestManagerRoutesWindowsUserToTasks(t *testing.T) {
	t.Parallel()
	h := newHarness("windows", "windows-service", false)
	spec := windowsUserSpec()
	h.runner.reply = func(name string, args []string) (string, int, error) {
		if name == "schtasks.exe" && args[0] == "/Query" {
			return "", 1, nil
		}
		return "", 0, nil
	}
	spec.LogFile = t.TempDir() + "/w.log"
	if err := h.m.Install(t.Context(), spec); err != nil {
		t.Fatalf("a non-elevated user must be able to install the task: %v", err)
	}
	if len(h.svc.calls) != 0 {
		t.Errorf("kardianos service used for a user task: %v", h.svc.calls)
	}
	info := h.m.Info(spec)
	if !strings.Contains(info.Platform, "Scheduled Task") || !strings.Contains(info.LogHint, "w.log") {
		t.Errorf("info = %+v", info)
	}
}
