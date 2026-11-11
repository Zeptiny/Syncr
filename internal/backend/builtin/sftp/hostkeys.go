package sftp

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// MaxHostKeys bounds the entries of the host_keys field: rclone pins at
// most 16 keys.
const MaxHostKeys = 16

// hostKeyEntries splits a host_keys value into its entries: one per line
// or separated by commas (rclone's form), surrounding white space removed,
// empty entries dropped.
func hostKeyEntries(v string) []string {
	var out []string
	for e := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// checkHostKeys returns the problems of a host_keys value, for people and
// without any part of the value: more than MaxHostKeys entries, or an entry
// that is not "<algorithm> <base64 key>" with a key whose own algorithm
// name equals <algorithm>.
func checkHostKeys(v string) []string {
	entries := hostKeyEntries(v)
	if len(entries) > MaxHostKeys {
		return []string{fmt.Sprintf("must have at most %d keys", MaxHostKeys)}
	}
	var out []string
	for i, e := range entries {
		if msg := checkHostKey(e); msg != "" {
			out = append(out, fmt.Sprintf("key %d: %s", i+1, msg))
		}
	}
	return out
}

// checkHostKey returns what is wrong with one host_keys entry, or "". The
// key is the SSH wire format of a public key (RFC 4253 section 6.6),
// base64-encoded: it starts with the algorithm name as a length-prefixed
// string, which must equal the entry's algorithm.
func checkHostKey(entry string) string {
	fields := strings.Fields(entry)
	if len(fields) != 2 {
		return `must be "<algorithm> <base64 key>", as in a known_hosts line without the host name and in the server's .pub file without the comment`
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "the key is not valid base64"
	}
	if len(blob) < 4 {
		return "the key is not an SSH public key"
	}
	n := binary.BigEndian.Uint32(blob)
	if n == 0 || int64(n) >= int64(len(blob))-4 {
		return "the key is not an SSH public key"
	}
	if string(blob[4:4+n]) != fields[0] {
		return "the algorithm does not match the key's type"
	}
	return ""
}

// normalizeHostKeys returns a valid host_keys value in rclone's form: the
// entries joined by commas, each "<algorithm> <base64 key>" with one space.
func normalizeHostKeys(v string) string {
	entries := hostKeyEntries(v)
	for i, e := range entries {
		entries[i] = strings.Join(strings.Fields(e), " ")
	}
	return strings.Join(entries, ",")
}
