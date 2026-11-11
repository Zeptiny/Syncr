package service

import (
	"errors"
	"strings"
	"testing"
)

func TestCommandLineProgram(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`"C:\Program Files\Syncr\syncr-worker.exe" run --config "C:\ProgramData\Syncr\worker.yaml"`: `C:\Program Files\Syncr\syncr-worker.exe`,
		`C:\Syncr\syncr-worker.exe run --config C:\ProgramData\Syncr\worker.yaml`:                   `C:\Syncr\syncr-worker.exe`,
		`C:\Syncr\syncr-worker.exe`:            `C:\Syncr\syncr-worker.exe`,
		"  \"C:\\a b\\w.exe\"\trun":            `C:\a b\w.exe`,
		`"C:\unterminated\w.exe`:               `C:\unterminated\w.exe`,
		"C:\\Syncr\\syncr-worker.exe\trun":     `C:\Syncr\syncr-worker.exe`,
		`"C:\dir\"`:                            `C:\dir\`, // backslashes are literal in the program name
		``:                                     ``,
		`"" run`:                               ``,
		`C:\PROGRA~1\Syncr\SYNCR-~1.EXE run`:   `C:\PROGRA~1\Syncr\SYNCR-~1.EXE`,
		`\\server\share\syncr-worker.exe run`:  `\\server\share\syncr-worker.exe`,
		`"\\?\C:\Syncr\syncr-worker.exe" run`:  `\\?\C:\Syncr\syncr-worker.exe`,
		`C:\Syncr\syncr-worker.exe --x "a b"`:  `C:\Syncr\syncr-worker.exe`,
		`  C:\Syncr\syncr-worker.exe  run   `:  `C:\Syncr\syncr-worker.exe`,
		`"C:\Syncr\syncr-worker.exe"run --x y`: `C:\Syncr\syncr-worker.exe`,
	}
	for in, want := range tests {
		if got := commandLineProgram(in); got != want {
			t.Errorf("commandLineProgram(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSystemdExecStart(t *testing.T) {
	t.Parallel()
	const unit = "syncr-worker.service"
	tests := []struct {
		name, out, want, wantErr string
	}{
		{"loaded", "LoadState=loaded\nExecStart={ path=/usr/local/bin/syncr-worker ; argv[]=/usr/local/bin/syncr-worker run --config /etc/syncr/worker.yaml ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n",
			"/usr/local/bin/syncr-worker", ""},
		{"path with a space", "ExecStart={ path=/opt/my tools/syncr-worker ; argv[]=/opt/my tools/syncr-worker run ; ignore_errors=no }\nLoadState=loaded",
			"/opt/my tools/syncr-worker", ""},
		{"not found", "LoadState=not-found\nExecStart=", "", "not installed"},
		{"no ExecStart", "LoadState=loaded\nExecStart=", "", "no ExecStart program"},
		{"empty path", "LoadState=loaded\nExecStart={ path= ; argv[]= }", "", "empty ExecStart program"},
	}
	for _, tt := range tests {
		got, err := parseSystemdExecStart(unit, tt.out)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: err = %v, want %q", tt.name, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: path = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
	if _, err := parseSystemdExecStart(unit, "LoadState=not-found"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("not-found: err = %v, want ErrNotInstalled", err)
	}
}

func TestManagerInstalledExecutable(t *testing.T) {
	t.Parallel()

	t.Run("systemd", func(t *testing.T) {
		t.Parallel()
		for _, scope := range []Scope{ScopeSystem, ScopeUser} {
			h := newHarness("linux", "linux-systemd", scope == ScopeSystem)
			h.runner.reply = func(string, []string) (string, int, error) {
				return "LoadState=loaded\nExecStart={ path=/srv/other/syncr-worker ; argv[]=/srv/other/syncr-worker run ; ignore_errors=no }", 0, nil
			}
			got, err := h.m.InstalledExecutable(t.Context(), testSpec(t, scope))
			if err != nil || got != "/srv/other/syncr-worker" {
				t.Errorf("%v: executable = %q, %v", scope, got, err)
			}
			want := "systemctl show -p LoadState -p ExecStart syncr-worker.service"
			if scope == ScopeUser {
				want = "systemctl --user show -p LoadState -p ExecStart syncr-worker.service"
			}
			if len(h.runner.calls) != 1 || h.runner.calls[0] != want {
				t.Errorf("%v: calls = %q, want %q", scope, h.runner.calls, want)
			}
		}
		h := newHarness("linux", "linux-systemd", true)
		h.runner.reply = func(string, []string) (string, int, error) { return "Failed to connect to bus", 1, nil }
		if _, err := h.m.InstalledExecutable(t.Context(), testSpec(t, ScopeSystem)); err == nil || !strings.Contains(err.Error(), "Failed to connect") {
			t.Errorf("failing systemctl: err = %v", err)
		}
	})

	t.Run("windows service", func(t *testing.T) {
		t.Parallel()
		spec := NewSpec("windows", ScopeSystem, `C:\Syncr\syncr-worker.exe`, `C:\ProgramData\Syncr\worker.yaml`, `C:\ProgramData\Syncr`, "")
		h := newHarness("windows", "windows-service", true)
		if _, err := h.m.InstalledExecutable(t.Context(), spec); !errors.Is(err, ErrNotInstalled) {
			t.Errorf("no service: err = %v, want ErrNotInstalled", err)
		}
		h.imagePath = `"C:\Program Files\Other\syncr-worker.exe" run --config "C:\ProgramData\Syncr\worker.yaml"`
		got, err := h.m.InstalledExecutable(t.Context(), spec)
		if err != nil || got != `C:\Program Files\Other\syncr-worker.exe` {
			t.Errorf("executable = %q, %v", got, err)
		}
		if len(h.svc.calls) != 0 || len(h.runner.calls) != 0 {
			t.Errorf("unexpected calls: service %v, commands %q", h.svc.calls, h.runner.calls)
		}
	})

	t.Run("windows user task", func(t *testing.T) {
		t.Parallel()
		h := newHarness("windows", "windows-service", false)
		h.runner.reply = func(name string, _ []string) (string, int, error) {
			if name == "schtasks.exe" {
				return "", 0, nil
			}
			return "QzpcdGFza1x3LmV4ZQ==", 0, nil // C:\task\w.exe
		}
		got, err := h.m.InstalledExecutable(t.Context(), windowsUserSpec())
		if err != nil || got != `C:\task\w.exe` {
			t.Errorf("executable = %q, %v", got, err)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		t.Parallel()
		h := newHarness("linux", "linux-openrc", true)
		if _, err := h.m.InstalledExecutable(t.Context(), testSpec(t, ScopeSystem)); !errors.Is(err, ErrUnsupported) {
			t.Errorf("openrc: err = %v, want ErrUnsupported", err)
		}
		h = newHarness("darwin", "darwin-launchd", true)
		spec := NewSpec("darwin", ScopeSystem, "/usr/local/bin/syncr-worker", "/etc/syncr/worker.yaml", "/var/lib/syncr", "")
		if _, err := h.m.InstalledExecutable(t.Context(), spec); !errors.Is(err, ErrUnsupported) {
			t.Errorf("launchd: err = %v, want ErrUnsupported", err)
		}
	})

	t.Run("invalid spec", func(t *testing.T) {
		t.Parallel()
		h := newHarness("linux", "linux-systemd", true)
		spec := testSpec(t, ScopeSystem)
		spec.Executable = "relative"
		if _, err := h.m.InstalledExecutable(t.Context(), spec); err == nil {
			t.Error("an invalid spec was accepted")
		}
	})
}
