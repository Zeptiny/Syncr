// Command pins prints the rclone and restic versions this release of the
// worker pins (tools.DefaultManifest), one "<tool> <version>" line per tool,
// for scripts/android/build-worker.sh. The manifest stays the single source
// of truth: the Android build compiles exactly these versions from source.
//
//	go run ./scripts/android/pins
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/Zeptiny/Syncr/internal/worker/tools"
)

func main() {
	if err := write(os.Stdout, tools.DefaultManifest()); err != nil {
		fmt.Fprintln(os.Stderr, "pins:", err)
		os.Exit(1)
	}
}

// Shapes the build script relies on: a tool name it can map to a source
// module, and a plain release version it can prefix with "v".
var (
	toolName    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	toolVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// errBadPin reports a manifest entry the build script could not use.
var errBadPin = errors.New("unusable pin")

// write prints one "<tool> <version>" line per tool of m, in manifest
// order. It fails on a name or version of another shape, so that the build
// script never builds something else than the manifest says.
func write(w io.Writer, m tools.Manifest) error {
	if len(m.Tools) == 0 {
		return fmt.Errorf("%w: the manifest has no tools", errBadPin)
	}
	for _, s := range m.Tools {
		if !toolName.MatchString(s.Name) || !toolVersion.MatchString(s.Version) {
			return fmt.Errorf("%w: %q %q", errBadPin, s.Name, s.Version)
		}
		if _, err := fmt.Fprintf(w, "%s %s\n", s.Name, s.Version); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	return nil
}
