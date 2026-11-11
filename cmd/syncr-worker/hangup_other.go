//go:build !unix

package main

// ignoreHangup does nothing where there is no SIGHUP. On Windows, closing
// the console window ends `upgrade` a few seconds later whatever it does;
// docs/upgrade.md says to keep the window open.
func ignoreHangup() {}
