// Command exitworker stands in for a syncr-worker release that cannot run:
// its "version" and "tools" commands succeed, so an upgrade installs it,
// but "run" exits at once, so the upgraded service does not stay up and
// the upgrade must roll back. The real-service upgrade test
// (cmd/syncr-worker/upgrade_service_integration_test.go) builds it.
package main

import (
	"fmt"
	"os"
	"runtime"
)

// Version is what "version" reports; the test may set it with -ldflags -X.
var Version = "v0.0.0-exits"

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("syncr-worker %s (commit none, built unknown, %s) %s/%s\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "tools":
		// "tools fetch" and "tools status": nothing to fetch, all found.
	default:
		fmt.Fprintln(os.Stderr, "exitworker: this release does not run")
		os.Exit(1)
	}
}
