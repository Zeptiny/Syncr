package service

import (
	"context"
	"fmt"
	"strings"
)

// The unit templates use the small template language of kardianos/service:
// {{Key}}, {{Key | fn}}, {{if Key}} and {{range Key}}. cmd and cmdEscape quote
// for the unit file, but escape only '"' and blanks; systemdEscaped escapes
// what systemd would otherwise expand before the values reach them.
//
// What the built-in template of kardianos/service gets wrong for this worker,
// and why a custom one is used: it hard-codes RestartSec=120, restarts even
// after a clean stop (Restart=always), and wants multi-user.target also for
// user units, which have no such target and would never start at login.
//
// KillMode=mixed sends the stop signal to the worker alone, as ADR 0005
// requires: the worker then stops its rclone and restic children itself
// (SIGINT, a grace period, then a kill) and reports their jobs as cancelled
// by the shutdown. The default, control-group, signals every process of the
// unit at once, so the tools would exit before the worker has cancelled their
// jobs, and the jobs would be reported as failed. Whatever is still running
// when the worker exits, or after TimeoutStopSec, is killed with SIGKILL.
// TimeoutStopSec leaves room for the worker's own stop budget (about 20 s).
//
// Hardening is deliberately mild. The worker is a backup tool: it must read
// and write whatever the operator listed in local_roots, so ProtectSystem=,
// ProtectHome= and PrivateTmp= (which hide parts of the file system from it)
// are left out. Operators who want a tighter sandbox can add a drop-in with
// "systemctl edit syncr-worker" (see docs/setup-linux.md).
const systemUnitTemplate = `[Unit]
Description={{Description}}
Documentation=https://github.com/Zeptiny/Syncr
After=network-online.target
Wants=network-online.target
ConditionFileIsExecutable={{Path | cmdEscape}}
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}Restart=on-failure
RestartSec=5
TimeoutStopSec=30
KillMode=mixed
NoNewPrivileges=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictRealtime=yes
LockPersonality=yes
{{range EnvVars}}{{.}}
{{end}}
[Install]
WantedBy=multi-user.target
`

// userUnitTemplate is the unit for the user manager: it starts with the
// user's session (default.target) and needs only the hardening that works
// without privileges.
const userUnitTemplate = `[Unit]
Description={{Description}}
Documentation=https://github.com/Zeptiny/Syncr
ConditionFileIsExecutable={{Path | cmdEscape}}
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}Restart=on-failure
RestartSec=5
TimeoutStopSec=30
KillMode=mixed
NoNewPrivileges=yes
{{range EnvVars}}{{.}}
{{end}}
[Install]
WantedBy=default.target
`

// systemdEscaped returns spec with its paths and arguments escaped for a
// unit file, which the template functions of kardianos/service (v1.3.0) do
// not do: systemd expands specifiers ("%h", "%2") in ExecStart=,
// ConditionFileIsExecutable= and WorkingDirectory=, so every '%' is
// doubled; it expands "$VAR" and "${VAR}" in the arguments of ExecStart=
// (not in its program path, nor in the other settings), so a '$' there is
// doubled too.
func systemdEscaped(spec Spec) Spec {
	pct := strings.NewReplacer("%", "%%")
	arg := strings.NewReplacer("%", "%%", "$", "$$")
	spec.Executable = pct.Replace(spec.Executable)
	spec.WorkDir = pct.Replace(spec.WorkDir)
	args := make([]string, len(spec.Args))
	for i, a := range spec.Args {
		args[i] = arg.Replace(a)
	}
	spec.Args = args
	return spec
}

func unitTemplate(scope Scope) string {
	if scope == ScopeUser {
		return userUnitTemplate
	}
	return systemUnitTemplate
}

// systemctlArgs prepends the flags that select the user manager.
func systemctlArgs(scope Scope, args ...string) []string {
	if scope == ScopeUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

// systemdState asks systemd for the unit's state. kardianos reports a unit
// that is waiting to restart after a crash as running; asking for SubState
// tells a restart loop from a healthy service.
func systemdState(ctx context.Context, r commandRunner, spec Spec) (State, error) {
	unit := spec.Name + ".service"
	out, code, err := r.Run(ctx, "systemctl", systemctlArgs(spec.Scope, "show", "-p", "LoadState", "-p", "ActiveState", "-p", "SubState", unit)...)
	if err != nil {
		return StateNotInstalled, err
	}
	if code != 0 {
		return StateNotInstalled, fmt.Errorf("systemctl show %s failed: %s%s", unit, out, userBusHint(spec.Scope, out))
	}
	props := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return parseSystemdState(props), nil
}

func parseSystemdState(props map[string]string) State {
	if props["LoadState"] == "not-found" {
		return StateNotInstalled
	}
	switch props["ActiveState"] {
	case "active", "reloading":
		return StateRunning
	case "activating":
		if strings.Contains(props["SubState"], "auto-restart") {
			return StateFailing
		}
		return StateRunning
	case "failed":
		return StateFailing
	default: // inactive, deactivating
		return StateStopped
	}
}

func userBusHint(scope Scope, output string) string {
	if scope == ScopeUser && strings.Contains(output, "bus") {
		return " (the user systemd instance is not reachable: log in to a session on this machine, or export XDG_RUNTIME_DIR=/run/user/$(id -u))"
	}
	return ""
}
