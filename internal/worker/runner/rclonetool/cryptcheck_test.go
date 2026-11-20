package rclonetool

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

func TestCryptCheckArgs(t *testing.T) {
	t.Parallel()
	want := []string{"cat", "--config=", "--use-json-log", "--log-level=NOTICE", "--count=1", "--min-size=1B", "--", "vault:"}
	if got := CryptCheckArgs("vault", "--config="); !slices.Equal(got, want) {
		t.Errorf("CryptCheckArgs = %q, want %q", got, want)
	}
	if got := CryptCheckArgs("r1"); got[0] != "cat" || got[1] != "--use-json-log" || got[len(got)-1] != "r1:" {
		t.Errorf("CryptCheckArgs without flags = %q", got)
	}
}

// recorded returns the lines of a recorded standard error of the check:
// testdata/rclone-<version>_cryptcheck_<storage>-<names>-<case>.jsonl,
// rclone 1.75.2 run as the check runs it (CryptCheckArgs, strict_names
// off) against MinIO, atmoz/sftp and "rclone serve webdav", without the
// stop (so a recording holds what rclone says until it ends), the
// timestamps removed. Re-record them on an rclone bump.
func recorded(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "rclone-1.75.2_cryptcheck_"+name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// TestCryptCheck: the verdict of rclone's output in each case recorded with
// rclone 1.75.2, when the check stops rclone, and what the failure message
// keeps. The data: photos/2024/a.jpg, photos/b.raw and docs/c.txt, written
// with the right password; "foreign" adds a README.md that the remote did
// not write in its docs folder; "deep" is one file 40 folders down; "huge"
// is the first 20 lines of 3,000 files.
func TestCryptCheck(t *testing.T) {
	t.Parallel()
	exit := func(code int) error { return &proc.ExitError{Path: "rclone", ExitCode: code} }
	type row struct {
		name    string
		stderr  string // recorded file, or "" for none
		stdout  bool   // a decrypted byte arrived
		code    int    // rclone's exit code, as recorded
		stopped bool
		want    CryptVerdict
		diag    string // in the diagnostics
	}
	rows := []row{
		// A block decrypted: the first byte stops rclone.
		{"readable", "", true, 0, true, CryptReadable, ""},
		// Encrypted names do not decrypt: skipped, with a notice each. On a
		// bucket the recursive listing names files (three: stopped); a
		// walk skips the folders at the root and descends no further.
		{"s3-standard-wrong", "s3-standard-wrong", false, 0, true, CryptUnreadable, "Skipping undecryptable file name"},
		{"s3-standard-wrong-huge", "s3-standard-wrong-huge", false, 0, true, CryptUnreadable, "Skipping undecryptable file name"},
		{"sftp-standard-wrong", "sftp-standard-wrong", false, 0, false, CryptUnreadable, "Skipping undecryptable dir name"},
		{"sftp-standard-wrong-deep", "sftp-standard-wrong-deep", false, 0, false, CryptUnreadable, "Skipping undecryptable dir name"},
		{"webdav-standard-wrong", "webdav-standard-wrong", false, 0, false, CryptUnreadable, "Skipping undecryptable dir name"},
		// Obfuscated names list garbled: on a bucket the files fail to
		// decrypt (three: stopped); walked folder by folder, garbled
		// folders are missing.
		{"s3-obfuscate-wrong", "s3-obfuscate-wrong", false, 1, true, CryptUnreadable, "bad password?"},
		{"sftp-obfuscate-wrong", "sftp-obfuscate-wrong", false, 3, false, CryptUnreadable, "directory not found"},
		{"sftp-obfuscate-wrong-deep", "sftp-obfuscate-wrong-deep", false, 3, false, CryptUnreadable, "directory not found"},
		{"webdav-obfuscate-wrong", "webdav-obfuscate-wrong", false, 3, false, CryptUnreadable, "directory not found"},
		// Names not encrypted: the files fail to decrypt.
		{"s3-off-wrong", "s3-off-wrong", false, 1, true, CryptUnreadable, "bad password?"},
		{"sftp-off-wrong", "sftp-off-wrong", false, 1, true, CryptUnreadable, "bad password?"},
		{"sftp-off-wrong-deep", "sftp-off-wrong-deep", false, 1, false, CryptUnreadable, "bad password?"},
		{"webdav-off-wrong", "webdav-off-wrong", false, 1, true, CryptUnreadable, "bad password?"},
		// Nothing stored yet: the folder (or the bucket) does not exist.
		{"sftp-missing-folder", "sftp-missing-folder", false, 3, false, CryptNothing, ""},
		{"webdav-missing-folder", "webdav-missing-folder", false, 3, false, CryptNothing, ""},
		{"s3-missing-bucket", "s3-missing-bucket", false, 3, false, CryptNothing, ""},
		// No file with data: nothing to decrypt.
		{"nothing", "", false, 0, false, CryptNothing, ""},
		// Anything else is the caller's to classify.
		{"failed", "", false, 5, false, CryptFailed, ""},
	}
	// The right password and a foreign file: skipped with a notice, and
	// the remote's own files decrypt.
	for _, storage := range []string{"s3", "sftp", "webdav"} {
		for _, names := range []string{"standard", "obfuscate", "off"} {
			name := storage + "-" + names + "-foreign"
			rows = append(rows, row{name, name, true, 0, true, CryptReadable, "README.md: Skipping undecryptable file name"})
		}
	}
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stops := 0
			tr := NewTracker(nil, scrub.New(nil))
			c := NewCryptCheck(tr, func() { stops++ })
			if tt.stderr != "" {
				for _, l := range recorded(t, tt.stderr) {
					c.Stderr(proc.Line{Data: []byte(l)})
				}
			}
			if tt.stdout {
				c.Stdout(proc.Line{Data: []byte("\xd5")})
			}
			var err error
			if tt.code != 0 {
				err = exit(tt.code)
			}
			if got := c.Verdict(tt.code, err); got != tt.want {
				t.Errorf("Verdict = %v, want %v", got, tt.want)
			}
			if (stops > 0) != tt.stopped || stops > 1 {
				t.Errorf("stop called %d times, want stopped=%v", stops, tt.stopped)
			}
			if d := strings.Join(tr.Diagnostics(), "; "); !strings.Contains(d, tt.diag) {
				t.Errorf("diagnostics = %q, want %q", d, tt.diag)
			}
		})
	}
}

// TestCryptCheckCutoff: the check stops rclone at the third sign of a
// wrong password, whatever the signs, and not before; a decrypted byte
// wins over any number of them, also after the stop (it raced it).
func TestCryptCheckCutoff(t *testing.T) {
	t.Parallel()
	skip := `{"level":"notice","msg":"Skipping undecryptable file name: illegal base32 data at input byte 6","object":"docs/README.md"}`
	bad := `{"level":"error","msg":"Failed to send to output: failed to authenticate decrypted block - bad password?","object":"a"}`
	missing := `{"level":"error","msg":"error listing: directory not found","object":"x"}`
	stops := 0
	c := NewCryptCheck(NewTracker(nil, nil), func() { stops++ })
	for i, l := range []string{skip, bad} {
		c.Stderr(proc.Line{Data: []byte(l)})
		if stops != 0 {
			t.Fatalf("stopped after %d signs", i+1)
		}
	}
	c.Stderr(proc.Line{Data: []byte(missing)})
	c.Stderr(proc.Line{Data: []byte(skip)})
	if stops != 1 {
		t.Fatalf("stop called %d times after four signs, want once", stops)
	}
	if got := c.Verdict(0, nil); got != CryptUnreadable {
		t.Errorf("Verdict = %v, want unreadable", got)
	}
	c.Stdout(proc.Line{Data: []byte("x")})
	if got := c.Verdict(-1, &proc.CanceledError{Path: "rclone", ExitCode: -1}); got != CryptReadable || stops != 1 {
		t.Errorf("a byte after the stop: Verdict = %v, stops %d; want readable, 1", got, stops)
	}

	// Two foreign names, then the remote's own data: readable.
	stops = 0
	c = NewCryptCheck(NewTracker(nil, nil), func() { stops++ })
	c.Stderr(proc.Line{Data: []byte(skip)})
	c.Stderr(proc.Line{Data: []byte(skip)})
	c.Stdout(proc.Line{Data: []byte("x")})
	c.Stderr(proc.Line{Data: []byte(skip)}) // after the byte: not a sign
	if got := c.Verdict(-1, &proc.CanceledError{Path: "rclone", ExitCode: -1}); got != CryptReadable || stops != 1 {
		t.Errorf("two foreign names, then data: Verdict = %v, stops %d; want readable, 1", got, stops)
	}
}

// TestCryptCheckSignals: which lines are signs of a wrong password, which
// say that nothing is stored yet, and which are neither. A file that fails
// for another reason, or a message that only holds a sign's words in a
// file name, is neither; the first decrypted byte wins over any of them.
func TestCryptCheckSignals(t *testing.T) {
	t.Parallel()
	line := func(level, msg, object string) proc.Line {
		return proc.Line{Data: []byte(`{"level":"` + level + `","msg":"` + msg + `","object":"` + object + `"}`)}
	}
	for _, tt := range []struct {
		name string
		l    proc.Line
		code int // rclone's exit code
		want CryptVerdict
	}{
		{"bad block on open", line("error", "Failed to open: failed to authenticate decrypted block - bad password?", "a"), 1, CryptUnreadable},
		{"bad block on output", line("error", "Failed to send to output: failed to authenticate decrypted block - bad password?", "a"), 1, CryptUnreadable},
		{"skipped file name", line("notice", "Skipping undecryptable file name: bad PKCS#7 padding - too long", "a"), 0, CryptUnreadable},
		{"skipped dir name", line("notice", "Skipping undecryptable dir name: bad PKCS#7 padding - too long", "a"), 0, CryptUnreadable},
		// With strict_names on (rclone's errors, should the check's
		// environment not turn it off).
		{"undecryptable names, listing", line("error", "error listing: there were 1 undecryptable name errors. first error: a: undecryptable file name detected: x", ""), 1, CryptUnreadable},
		{"undecryptable names, final", line("notice", "Failed to cat: there were 3 undecryptable name errors. first error: a: x", ""), 1, CryptUnreadable},
		{"undecryptable names, last error", line("notice", "Failed to cat with 2 errors: last error was: there were 2 undecryptable name errors. first error: a: x", ""), 1, CryptUnreadable},
		{"garbled folder missing", line("error", "error listing: directory not found", "x/y"), 3, CryptUnreadable},
		// The remote's folder or bucket is missing: nothing stored, but
		// only with exit code 3 (rclone's "directory not found").
		{"root missing", line("error", "error listing: directory not found", ""), 3, CryptNothing},
		{"bucket missing", line("notice", "Failed to cat: directory not found", ""), 3, CryptNothing},
		{"root missing, another exit", line("error", "error listing: directory not found", ""), 1, CryptFailed},
		{"no object field", proc.Line{Data: []byte(`{"level":"error","msg":"error listing: directory not found"}`)}, 3, CryptFailed},
		{"exit 3, unknown text", line("error", "error listing: object vanished", ""), 3, CryptFailed},
		{"exit 3, nothing said", proc.Line{}, 3, CryptFailed},
		// Neither.
		{"network", line("error", "Failed to open: connection reset by peer", "a"), 1, CryptFailed},
		{"not an encrypted file", line("error", "Failed to send to output: not an encrypted file - bad magic string", "a"), 1, CryptFailed},
		{"a file named like a sign", line("error", "Failed to open: permission denied", "undecryptable bad password"), 1, CryptFailed},
		{"a folder named like a sign", line("error", `error listing \"undecryptable-notes\": permission denied`, ""), 1, CryptFailed},
		{"a path in the message", line("error", "Failed to open: open /x/undecryptable name errors/bad password?: permission denied", "a"), 1, CryptFailed},
		{"plain text", proc.Line{Data: []byte("2026/11/20 ERROR : bad password")}, 1, CryptFailed},
		{"truncated", proc.Line{Data: []byte(`{"level":"error","msg":"Failed to open: failed to authenticate decrypted block - bad password?"}`), Truncated: true}, 1, CryptFailed},
	} {
		c := NewCryptCheck(NewTracker(nil, nil), func() {})
		c.Stderr(tt.l)
		var err error
		if tt.code != 0 {
			err = errors.New("exit")
		}
		if got := c.Verdict(tt.code, err); got != tt.want {
			t.Errorf("%s: Verdict = %v, want %v", tt.name, got, tt.want)
		}
		c.Stdout(proc.Line{Data: []byte("x")})
		if got := c.Verdict(tt.code, err); got != CryptReadable {
			t.Errorf("%s, then a decrypted byte: Verdict = %v, want readable", tt.name, got)
		}
	}
}

// TestCryptCheckEnv: the check's environment is a copy with the alias's
// strict_names off; the job's variables are not changed.
func TestCryptCheckEnv(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"RCLONE_CONFIG_MY_VAULT_TYPE": "crypt", "RCLONE_CONFIG_MY_VAULT_STRICT_NAMES": "true"}
	got := CryptCheckEnv(vars, "my_vault")
	if got["RCLONE_CONFIG_MY_VAULT_STRICT_NAMES"] != "false" || got["RCLONE_CONFIG_MY_VAULT_TYPE"] != "crypt" || len(got) != 2 {
		t.Errorf("CryptCheckEnv = %v", got)
	}
	if vars["RCLONE_CONFIG_MY_VAULT_STRICT_NAMES"] != "true" {
		t.Errorf("the job's variables changed: %v", vars)
	}
	if got := CryptCheckEnv(nil, "v1"); got["RCLONE_CONFIG_V1_STRICT_NAMES"] != "false" {
		t.Errorf("CryptCheckEnv(nil) = %v", got)
	}
}

func TestCryptUnreadableError(t *testing.T) {
	t.Parallel()
	err := CryptUnreadableError("source", []string{"a: Failed to open: bad password?", "b: x"})
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED {
		t.Errorf("code = %v", runner.CodeOf(err))
	}
	msg := err.Error()
	for _, want := range []string{"source: the encrypted remote cannot decrypt", "password is wrong", "nothing was read or written", "rclone: a: Failed to open: bad password?; b: x"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if msg := CryptUnreadableError("", nil).Error(); !strings.HasPrefix(msg, "the encrypted remote cannot decrypt") || strings.Contains(msg, "rclone:") {
		t.Errorf("message without a name or diagnostics = %q", msg)
	}
}
