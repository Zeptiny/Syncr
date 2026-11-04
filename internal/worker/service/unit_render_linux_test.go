//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	kservice "github.com/kardianos/service"
)

// renderUnit has kardianos/service render a unit template into a temporary
// home directory, exactly as Install does, and returns the unit file. The
// systemctl calls that follow the write fail because PATH is empty, so
// nothing is enabled or started; the test only reads the file.
func renderUnit(t *testing.T, template string, spec Spec) string {
	t.Helper()
	if kservice.Platform() != "linux-systemd" {
		t.Skipf("service manager is %q, not systemd", kservice.Platform())
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	exe := filepath.Join(home, "syncr worker") // a space exercises the quoting
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec.Executable = exe
	svc, err := kservice.New(noopProgram{}, &kservice.Config{
		Name: spec.Name, DisplayName: spec.DisplayName, Description: spec.Description,
		Executable: spec.Executable, Arguments: spec.Args, WorkingDirectory: spec.WorkDir,
		Option: kservice.KeyValue{"SystemdScript": template, "UserService": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Install() // fails at "systemctl enable" (no PATH) after writing the unit
	body, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", spec.Name+".service"))
	if err != nil {
		t.Fatalf("the unit was not written: %v", err)
	}
	return string(body)
}

func TestUnitTemplatesRender(t *testing.T) { //nolint:paralleltest // changes HOME and PATH
	spec := NewSpec("linux", ScopeSystem, "/unused", "/etc/syncr/worker.yaml", "/var/lib/syncr", "")
	spec.Args = []string{"run", "--config", "/etc/syncr/my worker.yaml"}

	t.Run("system", func(t *testing.T) {
		unit := renderUnit(t, systemUnitTemplate, spec)
		for _, want := range []string{
			"Description=Runs Syncr rclone and restic jobs",
			`/syncr\x20worker "run" "--config" "/etc/syncr/my worker.yaml"`,
			"WorkingDirectory=/var/lib/syncr",
			"After=network-online.target",
			"Restart=on-failure\nRestartSec=5\n",
			"StartLimitIntervalSec=300",
			"NoNewPrivileges=yes",
			"WantedBy=multi-user.target",
		} {
			if !strings.Contains(unit, want) {
				t.Errorf("unit lacks %q:\n%s", want, unit)
			}
		}
		for _, unwanted := range []string{"RestartSec=120", "Restart=always", "PrivateTmp", "ProtectSystem", "ProtectHome", "{{"} {
			if strings.Contains(unit, unwanted) {
				t.Errorf("unit contains %q:\n%s", unwanted, unit)
			}
		}
	})
	t.Run("user", func(t *testing.T) {
		unit := renderUnit(t, userUnitTemplate, spec)
		if !strings.Contains(unit, "WantedBy=default.target") || strings.Contains(unit, "multi-user.target") || strings.Contains(unit, "network-online") {
			t.Errorf("user unit:\n%s", unit)
		}
		if !strings.Contains(unit, "Restart=on-failure") || strings.Contains(unit, "{{") {
			t.Errorf("user unit:\n%s", unit)
		}
	})
}
