package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// InstalledExecutable implements Manager. It reads the installed service's
// definition, not spec.Executable:
//
//   - systemd (system and user units): the path of the unit's ExecStart,
//     from "systemctl show";
//   - Windows Service: the program of its command line (ImagePath) in the
//     Service Control Manager;
//   - Windows per-user task: the program of the task's action.
//
// Other service managers (SysV init, OpenRC, launchd) return ErrUnsupported.
func (m *manager) InstalledExecutable(ctx context.Context, spec Spec) (string, error) {
	if err := spec.validate(m.goos); err != nil {
		return "", err
	}
	switch {
	case m.useTask(spec):
		return m.tasks.executable(ctx, spec)
	case m.goos == "windows":
		line, err := m.serviceCommandLine(spec.Name)
		if err != nil {
			return "", err
		}
		exe := commandLineProgram(line)
		if exe == "" {
			return "", fmt.Errorf("the service %s has no program in its command line %q", spec.Name, line)
		}
		return exe, nil
	case m.goos == "linux" && m.isSystemd():
		return systemdExecutable(ctx, m.runner, spec)
	default:
		return "", fmt.Errorf("%w: the program of an installed service cannot be read from %s", ErrUnsupported, m.platform())
	}
}

// commandLineProgram returns the program of a Windows command line: its
// first argument as CommandLineToArgvW splits it, where the program name
// ends at the closing quote or at the first blank and backslashes are
// literal.
func commandLineProgram(line string) string {
	line = strings.TrimLeft(line, " \t")
	if rest, ok := strings.CutPrefix(line, `"`); ok {
		prog, _, _ := strings.Cut(rest, `"`)
		return prog
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i]
	}
	return line
}

// systemdExecutable reads the program of the unit's ExecStart.
func systemdExecutable(ctx context.Context, r commandRunner, spec Spec) (string, error) {
	unit := spec.Name + ".service"
	out, code, err := r.Run(ctx, "systemctl", systemctlArgs(spec.Scope, "show", "-p", "LoadState", "-p", "ExecStart", unit)...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("systemctl show %s failed: %s%s", unit, out, userBusHint(spec.Scope, out))
	}
	return parseSystemdExecStart(unit, out)
}

// parseSystemdExecStart reads the output of "systemctl show -p LoadState
// -p ExecStart", where ExecStart looks like
//
//	ExecStart={ path=/usr/local/bin/syncr-worker ; argv[]=/usr/local/bin/syncr-worker run ... ; ignore_errors=no ; ... }
//
// and returns the path. systemd prints the path unescaped; " ; " ends it.
func parseSystemdExecStart(unit, out string) (string, error) {
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] == "not-found" {
		return "", fmt.Errorf("%s: %w", unit, ErrNotInstalled)
	}
	_, rest, ok := strings.Cut(props["ExecStart"], "path=")
	if !ok {
		return "", fmt.Errorf("the unit %s has no ExecStart program (systemctl show printed %q)", unit, props["ExecStart"])
	}
	path, _, _ := strings.Cut(rest, " ;")
	if path = strings.TrimSpace(path); path == "" {
		return "", fmt.Errorf("the unit %s has an empty ExecStart program", unit)
	}
	return path, nil
}

// executable reads the program of the task's (first) action. PowerShell
// prints it base64-encoded UTF-8, so a path outside the console's code page
// survives the pipe.
func (t *taskManager) executable(ctx context.Context, spec Spec) (string, error) {
	_, code, err := t.schtasks(ctx, "/Query", "/TN", spec.Name)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("%s: %w", spec.Name, ErrNotInstalled)
	}
	script := "$a = @((Get-ScheduledTask -TaskPath '\\' -TaskName '" + strings.ReplaceAll(spec.Name, "'", "''") + "' -ErrorAction Stop).Actions)[0]; " +
		"[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$a.Execute))"
	out, code, err := t.runner.Run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("read the scheduled task's action: %s", out)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return "", fmt.Errorf("read the scheduled task's action: unexpected output %q: %w", out, err)
	}
	// Task Scheduler accepts a quoted program too.
	exe := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if exe == "" {
		return "", errors.New("the scheduled task has no program in its action")
	}
	return exe, nil
}
