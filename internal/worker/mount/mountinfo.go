package mount

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// Entry is one line of /proc/<pid>/mountinfo (proc(5)) that this
// package reads.
type Entry struct {
	// Point is the mount point, unescaped, as seen in the process's mount
	// namespace.
	Point string
	// FSType is the filesystem type, e.g. "fuse.rclone".
	FSType string
	// Source is the mount source, e.g. the --devname of an rclone mount.
	Source string
}

// ParseTable parses the content of /proc/<pid>/mountinfo. Lines that do
// not have the documented form are skipped. A line reads:
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// The mount point is the fifth field; the optional fields end at "-",
// followed by the filesystem type and the source. The kernel escapes space,
// tab, newline and backslash in paths as \040, \011, \012 and \134.
func ParseTable(data []byte) []Entry {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 7 {
			continue
		}
		info := Entry{Point: unescapeMountField(fields[4])}
		for i := 6; i < len(fields); i++ {
			if fields[i] != "-" {
				continue
			}
			if i+1 < len(fields) {
				info.FSType = fields[i+1]
			}
			if i+2 < len(fields) {
				info.Source = unescapeMountField(fields[i+2])
			}
			break
		}
		out = append(out, info)
	}
	return out
}

// unescapeMountField undoes the kernel's octal escapes (\NNN) in a field of
// the mount table.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountedAt reports whether the mount table lists point (an absolute,
// symlink-free path) as a mount point.
func mountedAt(infos []Entry, point string) bool {
	for _, mi := range infos {
		if mi.Point == point {
			return true
		}
	}
	return false
}

// mountsBelow returns the mount points that the mount table lists directly
// inside dir (an absolute, symlink-free path), in table order, each once.
func mountsBelow(infos []Entry, dir string) []string {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	seen := map[string]bool{}
	for _, mi := range infos {
		rest, ok := strings.CutPrefix(mi.Point, prefix)
		if !ok || rest == "" || strings.Contains(rest, "/") || seen[mi.Point] {
			continue
		}
		seen[mi.Point] = true
		out = append(out, mi.Point)
	}
	return out
}
