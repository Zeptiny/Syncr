package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPruneRemovesOnlyOtherManagedVersions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kept := []string{
		mk("rclone/1.75.2/linux-amd64/rclone"),
		mk("rclone/1.75.2/windows-amd64/rclone.exe"),
		mk("restic/0.19.1/linux-amd64/restic"),
		mk("other/0.1.0/linux-amd64/other"), // a tool keep does not name
		mk("rclone/README"),                 // a file, not a version directory
		mk(".lock"),
	}
	mk("rclone/1.70.0/linux-amd64/rclone")
	mk("rclone/1.74.0/windows-amd64/rclone.exe")
	mk("restic/0.18.0/linux-amd64/restic")
	// A symbolic link to a directory elsewhere is not followed or removed.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep-me")
	if err := os.WriteFile(outsideFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "restic", "0.17.0")
	if err := os.Symlink(outside, link); err != nil {
		t.Logf("no symlink test: %v", err) // Windows without the privilege
		link = ""
	}

	inst := offlineInstaller(t, dir)
	removed, err := inst.Prune(t.Context(), pinsKept())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(dir, "rclone", "1.70.0"),
		filepath.Join(dir, "rclone", "1.74.0"),
		filepath.Join(dir, "restic", "0.18.0"),
	}
	slices.Sort(removed)
	if !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q", removed, want)
	}
	for _, p := range want {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", p, err)
		}
	}
	for _, p := range append(kept, outsideFile) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if link != "" {
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("symbolic link removed: %v", err)
		}
	}

	// Nothing left to prune: a second run removes nothing.
	removed, err = inst.Prune(t.Context(), pinsKept())
	if err != nil || len(removed) != 0 {
		t.Errorf("second prune = %q, %v", removed, err)
	}
}

// DefaultManifestPinsForTest pins the versions the first prune kept.
func pinsKept() []ToolVersion {
	return []ToolVersion{{Tool: "rclone", Version: "1.75.2"}, {Tool: "restic", Version: "0.19.1"}}
}

func TestPruneRefusesBadInput(t *testing.T) {
	t.Parallel()
	inst := offlineInstaller(t, t.TempDir())
	for name, keep := range map[string][]ToolVersion{
		"nothing to keep": nil,
		"empty version":   {{Tool: "rclone"}},
		"path in version": {{Tool: "rclone", Version: "../x"}},
		"dot version":     {{Tool: "rclone", Version: ".."}},
		"path in tool":    {{Tool: "../rclone", Version: "1.0.0"}},
		"repeated tool":   {{Tool: "rclone", Version: "1"}, {Tool: "rclone", Version: "2"}},
	} {
		if _, err := inst.Prune(t.Context(), keep); err == nil {
			t.Errorf("%s: Prune(%v) succeeded", name, keep)
		}
	}
}

func TestPruneWaitsForTheInstallLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	inst := offlineInstaller(t, dir)
	release, err := inst.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := inst.Prune(ctx, pinsKept()); err == nil {
		t.Error("Prune ran while an install held the lock")
	}
}

func TestManifestPins(t *testing.T) {
	t.Parallel()
	got := DefaultManifest().Pins()
	if len(got) != 2 || got[0].Tool != "rclone" || got[1].Tool != "restic" || got[0].Version == "" || got[1].Version == "" {
		t.Errorf("Pins() = %v", got)
	}
}

// offlineInstaller returns an installer that never downloads.
func offlineInstaller(t *testing.T, dir string) *Installer {
	t.Helper()
	inst, err := NewInstaller(InstallerOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}
