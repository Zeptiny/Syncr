//go:build unix

package main

import (
	"os/signal"
	"syscall"
)

// ignoreHangup keeps `upgrade` running when its terminal goes away (an SSH
// connection that drops): SIGHUP would end it between the swap and the
// health check, without a rollback. SIGPIPE is ignored too, so that output
// to a pipe whose reader died with the session (`| tee`) fails quietly
// instead of ending the process. Interrupt and SIGTERM keep their meaning:
// the upgrade ignores them only after the swap, and then finishes.
func ignoreHangup() { signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE) }
