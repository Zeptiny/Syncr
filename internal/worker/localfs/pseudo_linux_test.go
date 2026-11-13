package localfs

import (
	"os"
	"testing"
)

// TestPseudoFS tells proc and sysfs from a folder on a disk by their
// filesystem types, and measuring a root on proc counts nothing (no
// /proc/kcore of the address space's size).
func TestPseudoFS(t *testing.T) {
	t.Parallel()
	for dir, want := range map[string]bool{"/proc": true, "/sys": true, realTempDir(t): false} {
		f, err := os.Open(dir)
		if err != nil {
			t.Skipf("%s: %v", dir, err)
		}
		if got := pseudoFS(f); got != want {
			t.Errorf("pseudoFS(%s) = %v, want %v", dir, got, want)
		}
		_ = f.Close()
	}
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("no /proc")
	}
	p := mustPolicy(t, "/proc")
	m, err := p.Measure(t.Context(), "/proc", 10)
	if err != nil || m.Bytes != 0 || m.Files != 0 || !m.Complete {
		t.Errorf("measuring /proc = %+v, %v", m, err)
	}
}
