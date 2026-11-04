//go:build unix

package proc

// allowedEnvNames lists the worker variables children may inherit on Unix.
func allowedEnvNames() []string {
	return append(commonEnvNames(), "PATH", "HOME", "USER", "LOGNAME", "TMPDIR")
}

// envNameEqual compares variable names; Unix names are case-sensitive.
func envNameEqual(a, b string) bool { return a == b }
