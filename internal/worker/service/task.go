package service

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// How long a stop waits for the task's instance to end, and how often it
// looks.
const (
	taskStopTimeout = 30 * time.Second
	taskStopPoll    = 500 * time.Millisecond
)

// taskManager runs the worker as a Scheduled Task that starts at the user's
// logon. It drives schtasks.exe and PowerShell, which exist on every
// supported Windows, through a commandRunner so the command lines can be
// tested anywhere.
//
// Why a task and not a service for users without administrator rights:
// creating a Windows Service needs administrator rights, whereas any user may
// register a task that runs as themselves. A task with a logon trigger bound
// to the user's own account (<UserId> in the XML; the schtasks /SC ONLOGON
// shorthand would create an "any user" trigger that needs administrator
// rights) starts the worker at every logon, with the user's rights and
// without a password prompt.
type taskManager struct {
	runner  commandRunner
	user    func() (string, error)
	tempDir func() string
	// sleep waits d or until ctx ends.
	sleep func(ctx context.Context, d time.Duration) error
}

// taskDocument is the Task Scheduler 1.2 XML schema, reduced to what the
// worker needs.
type taskDocument struct {
	XMLName xml.Name `xml:"Task"`
	Version string   `xml:"version,attr"`
	Xmlns   string   `xml:"xmlns,attr"`

	Description string `xml:"RegistrationInfo>Description"`

	LogonTrigger struct {
		Enabled bool   `xml:"Enabled"`
		UserID  string `xml:"UserId"`
	} `xml:"Triggers>LogonTrigger"`

	Principal struct {
		ID        string `xml:"id,attr"`
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`

	Settings struct {
		MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
		DisallowStartIfOnBatteries bool   `xml:"DisallowStartIfOnBatteries"`
		StopIfGoingOnBatteries     bool   `xml:"StopIfGoingOnBatteries"`
		AllowHardTerminate         bool   `xml:"AllowHardTerminate"`
		StartWhenAvailable         bool   `xml:"StartWhenAvailable"`
		AllowStartOnDemand         bool   `xml:"AllowStartOnDemand"`
		Enabled                    bool   `xml:"Enabled"`
		Hidden                     bool   `xml:"Hidden"`
		ExecutionTimeLimit         string `xml:"ExecutionTimeLimit"`
		RestartOnFailure           struct {
			Interval string `xml:"Interval"`
			Count    int    `xml:"Count"`
		} `xml:"RestartOnFailure"`
	} `xml:"Settings"`

	Actions struct {
		Context string `xml:"Context,attr"`
		Exec    struct {
			Command          string `xml:"Command"`
			Arguments        string `xml:"Arguments"`
			WorkingDirectory string `xml:"WorkingDirectory,omitempty"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

// taskXML renders the task definition for spec, bound to account (as
// "DOMAIN\name"), encoded the way schtasks /XML expects: UTF-16 little endian
// with a byte order mark.
func taskXML(spec Spec, account string) ([]byte, error) {
	var doc taskDocument
	doc.Version = "1.2"
	doc.Xmlns = "http://schemas.microsoft.com/windows/2004/02/mit/task"
	doc.Description = spec.Description
	doc.LogonTrigger.Enabled = true
	doc.LogonTrigger.UserID = account
	doc.Principal.ID = "Author"
	doc.Principal.UserID = account
	doc.Principal.LogonType = "InteractiveToken"
	doc.Principal.RunLevel = "LeastPrivilege"
	s := &doc.Settings
	s.MultipleInstancesPolicy = "IgnoreNew"
	s.AllowHardTerminate = true
	s.StartWhenAvailable = true
	s.AllowStartOnDemand = true
	s.Enabled = true
	s.ExecutionTimeLimit = "PT0S" // no time limit
	s.RestartOnFailure.Interval = "PT1M"
	s.RestartOnFailure.Count = 999
	doc.Actions.Context = doc.Principal.ID
	doc.Actions.Exec.Command = spec.Executable
	doc.Actions.Exec.Arguments = windowsCommandLine(spec.Args)
	doc.Actions.Exec.WorkingDirectory = spec.WorkDir

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the task definition: %w", err)
	}
	text := `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + string(body) + "\n"
	return encodeUTF16LE(text), nil
}

func encodeUTF16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE // byte order mark
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// windowsCommandLine joins args into one command line using the quoting rules
// of CommandLineToArgvW.
func windowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windowsQuote(a)
	}
	return strings.Join(quoted, " ")
}

func windowsQuote(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		switch r {
		case '\\':
			backslashes++
			b.WriteRune(r)
		case '"':
			b.WriteString(strings.Repeat(`\`, backslashes+1))
			b.WriteRune(r)
			backslashes = 0
		default:
			backslashes = 0
			b.WriteRune(r)
		}
	}
	b.WriteString(strings.Repeat(`\`, backslashes)) // before the closing quote
	b.WriteByte('"')
	return b.String()
}

func (t *taskManager) schtasks(ctx context.Context, args ...string) (string, int, error) {
	return t.runner.Run(ctx, "schtasks.exe", args...)
}

func (t *taskManager) install(ctx context.Context, spec Spec) error {
	account, err := t.user()
	if err != nil {
		return err
	}
	body, err := taskXML(spec, account)
	if err != nil {
		return err
	}
	if spec.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(spec.LogFile), 0o700); err != nil {
			return fmt.Errorf("create the log directory: %w", err)
		}
	}
	f, err := os.CreateTemp(t.tempDir(), "syncr-task-*.xml")
	if err != nil {
		return fmt.Errorf("write the task definition: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, werr := f.Write(body)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write the task definition: %w", werr)
	}
	out, code, err := t.schtasks(ctx, "/Create", "/TN", spec.Name, "/XML", f.Name())
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("create the scheduled task: schtasks exited with code %d: %s", code, out)
	}
	return nil
}

func (t *taskManager) uninstall(ctx context.Context, spec Spec) error {
	// Not running is fine, and a worker that does not end in time is no
	// reason to keep the task.
	if _, code, err := t.schtasks(ctx, "/End", "/TN", spec.Name); err == nil && code == 0 {
		_ = t.waitEnded(ctx, spec)
	}
	out, code, err := t.schtasks(ctx, "/Delete", "/TN", spec.Name, "/F")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("delete the scheduled task: schtasks exited with code %d: %s", code, out)
	}
	return nil
}

// control starts, stops or restarts the task.
//
// schtasks /End returns once Task Scheduler was asked to end the instance,
// not when the worker's process has exited, so a stop waits until the
// task's state is no longer Running. A restart needs that: the task's
// MultipleInstancesPolicy is IgnoreNew, so a /Run while the old instance is
// still ending is dropped without an error, and the task would stay stopped.
// It also lets an upgrade replace the binary of a worker that has exited.
func (t *taskManager) control(ctx context.Context, spec Spec, action string) error {
	if action == "restart" {
		if err := t.control(ctx, spec, "stop"); err != nil {
			return err
		}
		return t.control(ctx, spec, "start")
	}
	verb := "/Run"
	if action == "stop" {
		verb = "/End"
	}
	out, code, err := t.schtasks(ctx, verb, "/TN", spec.Name)
	if err != nil {
		return err
	}
	if code != 0 {
		if action == "stop" && strings.Contains(strings.ToLower(out), "not currently running") {
			return nil
		}
		return fmt.Errorf("%s the scheduled task: schtasks exited with code %d: %s", action, code, out)
	}
	if action == "stop" {
		return t.waitEnded(ctx, spec)
	}
	return nil
}

// waitEnded waits until the task's instance has ended (its state is no
// longer Running), for at most taskStopTimeout.
func (t *taskManager) waitEnded(ctx context.Context, spec Spec) error {
	for waited := time.Duration(0); ; waited += taskStopPoll {
		running, err := t.running(ctx, spec)
		if err != nil {
			return fmt.Errorf("wait for the scheduled task to end: %w", err)
		}
		if !running {
			return nil
		}
		if waited >= taskStopTimeout {
			return fmt.Errorf("the scheduled task %s is still running %s after it was ended", spec.Name, taskStopTimeout)
		}
		if err := t.sleep(ctx, taskStopPoll); err != nil {
			return fmt.Errorf("wait for the scheduled task to end: %w", err)
		}
	}
}

// status queries existence with schtasks (its exit code does not depend on
// the Windows display language) and the state with PowerShell, whose state
// names are not localized either.
func (t *taskManager) status(ctx context.Context, spec Spec) (State, error) {
	_, code, err := t.schtasks(ctx, "/Query", "/TN", spec.Name)
	if err != nil {
		return StateNotInstalled, err
	}
	if code != 0 {
		return StateNotInstalled, nil
	}
	running, err := t.running(ctx, spec)
	if err != nil {
		return StateNotInstalled, err
	}
	if running {
		return StateRunning, nil
	}
	return StateStopped, nil
}

// running reports whether the task's state is Running.
func (t *taskManager) running(ctx context.Context, spec Spec) (bool, error) {
	script := "(Get-ScheduledTask -TaskName '" + strings.ReplaceAll(spec.Name, "'", "''") + "').State"
	out, code, err := t.runner.Run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("query the scheduled task state: %s", out)
	}
	return parseTaskState(out) == StateRunning, nil
}

func parseTaskState(out string) State {
	if strings.EqualFold(strings.TrimSpace(out), "Running") {
		return StateRunning
	}
	return StateStopped // Ready, Disabled, Queued, Unknown
}
