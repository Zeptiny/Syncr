//go:build windows

package localfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithinWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		root, path string
		want       bool
	}{
		{`C:\data`, `C:\data`, true},
		{`C:\data`, `c:\DATA\Sub\x`, true}, // case-insensitive volume and components
		{`C:\data`, `C:\data2`, false},
		{`C:\data`, `C:\data2\x`, false},
		{`C:\data`, `D:\data\x`, false},
		{`C:\`, `C:\anything`, true},
		{`C:\`, `D:\anything`, false},
		{`\\srv\share`, `\\SRV\Share\x\y`, true},
		{`\\srv\share`, `\\srv\share2\x`, false},
		{`\\srv\share\dir`, `\\srv\share`, false},
		{`C:\data`, `\\srv\share\data`, false},
	}
	for _, tt := range tests {
		if got := within(tt.root, tt.path); got != tt.want {
			t.Errorf("within(%q, %q) = %v, want %v", tt.root, tt.path, got, tt.want)
		}
	}
}

func TestCleanAbsWindows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string // empty: ErrInvalidPath expected
	}{
		{`C:\data\x`, `C:\data\x`},
		{`C:/data/x/`, `C:\data\x`},
		{`c:\data\..\x`, `c:\x`},
		{`\\srv\share\x`, `\\srv\share\x`},
		{`C:foo`, ""},                   // drive-relative
		{`C:`, ""},                      // drive-relative
		{`\foo`, ""},                    // rooted on the current drive
		{`/foo`, ""},                    // same, with slashes
		{`foo\bar`, ""},                 // relative
		{`\\?\C:\data`, ""},             // verbatim
		{`\\.\PhysicalDrive0`, ""},      // device
		{`//./C:/data`, ""},             // device, with slashes
		{`C:\data\NUL`, ""},             // reserved name
		{`C:\data\con.txt`, ""},         // reserved name with extension (a file on Windows 11)
		{`C:\data\CON .txt`, ""},        // trailing space before the extension
		{`C:\data\nul.`, ""},            // trailing dot
		{`C:\data\lpt1.log`, ""},        // port with extension
		{`C:\data\com¹`, ""},            // superscript digit
		{`C:\data\LPT³.txt`, ""},        // superscript digit with extension
		{`C:\data\conin$`, ""},          // console input
		{`C:\data\CONOUT$`, ""},         // console output
		{`C:\data\COM1\x`, ""},          // reserved name in the middle
		{`C:\x\aux\y`, ""},              // nested component
		{`C:\x\Prn.tar.gz\y`, ""},       // nested, double extension
		{`C:\data\file.txt:stream`, ""}, // alternate data stream
		{`C:\data\console`, `C:\data\console`},
		{`C:\data\conx.txt`, `C:\data\conx.txt`},
		{`C:\data\lpt10`, `C:\data\lpt10`},
		{`C:\data\COM10\x`, `C:\data\COM10\x`},
		{`C:\data\auxiliary\nul1`, `C:\data\auxiliary\nul1`},
		{`C:\data\file.con`, `C:\data\file.con`},
	}
	for _, tt := range tests {
		got, err := cleanAbs(tt.path)
		if tt.want == "" {
			if !errors.Is(err, ErrInvalidPath) {
				t.Errorf("cleanAbs(%q) = (%q, %v), want ErrInvalidPath", tt.path, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("cleanAbs(%q) = (%q, %v), want %q", tt.path, got, err, tt.want)
		}
	}
}

func TestStripVerbatim(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`\\?\C:\data`:         `C:\data`,
		`\\?\UNC\srv\share\x`: `\\srv\share\x`,
		`C:\plain`:            `C:\plain`,
		`\\?\Volume{1234}\x`:  `Volume{1234}\x`,
	} {
		if got := stripVerbatim(in); got != want {
			t.Errorf("stripVerbatim(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveCaseInsensitive(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	p := mustPolicy(t, root)
	for _, path := range []string{strings.ToUpper(root) + `\x`, strings.ToLower(root) + `\x`} {
		if got, err := p.Resolve(path); err != nil || got != path {
			t.Errorf("Resolve(%q) = (%q, %v)", path, got, err)
		}
	}
}

// junction creates a directory junction, which (unlike a symlink) needs no
// special privileges.
func junction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}

func TestResolveJunctions(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	mkdirs(t, filepath.Join(root, "real"), outside)
	junction(t, filepath.Join(root, "escape"), outside)
	junction(t, filepath.Join(base, "way-in"), filepath.Join(root, "real"))
	p := mustPolicy(t, root)

	for _, path := range []string{filepath.Join(root, "escape"), filepath.Join(root, "escape", "new", "x")} {
		if _, err := p.Resolve(path); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Resolve(%q) err = %v, want ErrNotAllowed (junction escape)", path, err)
		}
	}
	in := filepath.Join(base, "way-in", "x")
	if got, err := p.Resolve(in); err != nil || got != in {
		t.Errorf("Resolve(%q) = (%q, %v)", in, got, err)
	}
}

func TestResolveDanglingJunction(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	target := filepath.Join(base, "gone")
	mkdirs(t, root, target)
	junction(t, filepath.Join(root, "j"), target)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	p := mustPolicy(t, root)
	if _, err := p.Resolve(filepath.Join(root, "j", "x")); !errors.Is(err, ErrUnresolvable) {
		t.Errorf("err = %v, want ErrUnresolvable", err)
	}
}
