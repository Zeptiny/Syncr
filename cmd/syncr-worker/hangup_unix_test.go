//go:build unix

package main

import (
	"os/signal"
	"syscall"
	"testing"
)

// TestIgnoreHangup: `upgrade` survives the loss of its terminal. (It
// changes this test process's signal handling, which no other test uses.)
func TestIgnoreHangup(t *testing.T) {
	ignoreHangup()
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGPIPE} {
		if !signal.Ignored(sig) {
			t.Errorf("%v is not ignored", sig)
		}
	}
}
