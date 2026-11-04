package localfs

import "strings"

// isReservedName reports whether the path component name names a Windows
// device rather than a file: CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9, the
// superscript-digit ports (COM¹, COM², COM³, LPT¹, LPT², LPT³), CONIN$ and
// CONOUT$. It is only applied on Windows (see checkPlatform) but compiled on
// every platform so its tests run everywhere.
//
// The check is deliberately conservative and does not depend on the Windows
// version. Before Windows 11 a reserved name followed by any extension
// ("con.txt", "LPT1.tar.gz") opened the device; Windows 11 and Server 2025
// treat those as ordinary files, which is why filepath.IsLocal (it asks
// RtlIsDosDeviceName_U for names with an extension) accepts them there.
// Here everything from the first dot (or colon) on is ignored, then trailing
// spaces, which Win32 path normalization strips ("CON .txt", "nul."), and
// the rest is compared case-insensitively with the reserved names.
func isReservedName(name string) bool {
	base := name
	if i := strings.IndexAny(base, ".:"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(base, " ")
	if len(base) < 3 {
		return false
	}
	prefix, suffix := base[:3], base[3:]
	switch {
	case suffix == "":
		return strings.EqualFold(prefix, "CON") || strings.EqualFold(prefix, "PRN") ||
			strings.EqualFold(prefix, "AUX") || strings.EqualFold(prefix, "NUL")
	case strings.EqualFold(prefix, "COM") || strings.EqualFold(prefix, "LPT"):
		switch suffix {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
		return false
	case strings.EqualFold(prefix, "CON"):
		return strings.EqualFold(suffix, "IN$") || strings.EqualFold(suffix, "OUT$")
	}
	return false
}
