//go:build integration

package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// TestPinnedLinuxAmd64Artifacts downloads the real pinned rclone and restic
// archives, checks them against the embedded SHA-256s and, where the host can
// run them, confirms the extracted binaries report the pinned versions.
func TestPinnedLinuxAmd64Artifacts(t *testing.T) {
	target := Platform{OS: "linux", Arch: "amd64"}
	manifest := DefaultManifest()
	sup := proc.New(proc.Options{})
	dir := t.TempDir()

	var det VersionDetector
	if HostPlatform() == target {
		det = NewProcDetector(sup)
	}
	inst, err := NewInstaller(InstallerOptions{Dir: dir, Detector: det})
	if err != nil {
		t.Fatal(err)
	}

	for _, spec := range manifest.Tools {
		t.Run(spec.Name, func(t *testing.T) {
			got, err := inst.Install(t.Context(), spec, target)
			if err != nil {
				t.Fatal(err)
			}
			if got.Reused || got.Version != spec.Version {
				t.Errorf("Install = %+v", got)
			}
			info, err := os.Stat(got.Path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() < 10<<20 {
				t.Errorf("%s is only %d bytes", got.Path, info.Size())
			}
			if filepath.Base(got.Path) != spec.Name {
				t.Errorf("binary name %s", filepath.Base(got.Path))
			}

			again, err := inst.Install(t.Context(), spec, target)
			if err != nil || !again.Reused {
				t.Errorf("second Install = %+v, %v", again, err)
			}
		})
	}

	if HostPlatform() != target {
		t.Skipf("host is %s; downloaded and verified but did not execute the binaries", runtime.GOOS+"/"+runtime.GOARCH)
	}

	// End to end through the resolver, offline from here on.
	r, err := New(Config{Manifest: manifest, ToolsDir: dir, Runner: sup})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range manifest.Tools {
		res, err := r.Resolve(t.Context(), spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if res.Source != SourceManaged || res.Version != spec.Version {
			t.Errorf("%s resolved %+v", spec.Name, res)
		}
	}
}
