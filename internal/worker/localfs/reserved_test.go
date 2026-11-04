package localfs

import "testing"

func TestIsReservedName(t *testing.T) {
	t.Parallel()
	reserved := []string{
		"CON", "con", "Con", "PRN", "aux", "NUL", "nul",
		"COM0", "COM1", "com9", "LPT0", "lpt1", "LPT9",
		"COM¹", "com²", "LPT³", "lpt¹",
		"CONIN$", "conin$", "CONOUT$", "ConOut$",
		// with an extension: a device before Windows 11, a file since
		"con.txt", "CON.TXT", "lpt1.log", "nul.tar.gz", "aux.", "conin$.txt", "COM¹.txt",
		// trailing dots and spaces are stripped by Win32 normalization
		"nul.", "NUL..", "CON ", "CON .txt", "con  . txt", "COM1 ", "lpt1 .x",
		// a stream suffix (also rejected for the colon)
		"con:stream", "NUL::$DATA",
	}
	for _, name := range reserved {
		if !isReservedName(name) {
			t.Errorf("isReservedName(%q) = false, want true", name)
		}
	}
	allowed := []string{
		"", ".", "..", "co", "CO.txt",
		"console", "conx.txt", "CONS", "connect.log", "contoso",
		"lpt10", "COM10", "LPTX", "com", "lpt", "COM.txt", "com1x", "COM⁴", "LPT¹¹",
		"auxiliary", "nul1", "prn2", "null",
		"conin", "conin$x", "conout", "CONOUT", "con$", "CONERR$",
		" CON", "xCON", "a.con", "data.nul", "file.COM1",
		"CON x.txt", "nul_", "сon", // Cyrillic es
	}
	for _, name := range allowed {
		if isReservedName(name) {
			t.Errorf("isReservedName(%q) = true, want false", name)
		}
	}
}
