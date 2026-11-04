//go:build windows

package proc

import "strings"

// allowedEnvNames lists the worker variables children may inherit on
// Windows: the ones the OS, the C runtime and Go programs rely on to locate
// system directories, temp space and the user profile.
func allowedEnvNames() []string {
	return append(commonEnvNames(),
		"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "windir", "ComSpec",
		"TEMP", "TMP", "USERPROFILE", "USERNAME", "USERDOMAIN", "HOMEDRIVE", "HOMEPATH",
		"APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "CommonProgramFiles",
		"COMPUTERNAME", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS",
	)
}

// envNameEqual compares variable names; Windows names are case-insensitive.
func envNameEqual(a, b string) bool { return strings.EqualFold(a, b) }
