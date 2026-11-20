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

// recorded returns the lines of a recorded standard error of the check
// (rclone 1.75.2 against MinIO and atmoz/sftp, the timestamps removed).
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
// keeps.
func TestCryptCheck(t *testing.T) {
	t.Parallel()
	exit := func(code int) error { return &proc.ExitError{Path: "rclone", ExitCode: code} }
	for _, tt := range []struct {
		name    string
		stderr  string // recorded file, or "" for none
		stdout  []string
		code    int
		stopped bool
		want    CryptVerdict
		diag    string // in the diagnostics
	}{
		// A block decrypted: the first byte stops rclone.
		{"readable", "", []string{"\xd5"}, 0, true, CryptReadable, ""},
		// Obfuscated and unencrypted names list; three files that do not
		// decrypt stop rclone.
		{"s3-obfuscate-wrong", "s3-obfuscate-wrong", nil, 1, true, CryptUnreadable, "bad password?"},
		{"s3-off-wrong", "s3-off-wrong", nil, 1, true, CryptUnreadable, "bad password?"},
		// Encrypted names fail the recursive listing (strict_names).
		{"s3-standard-wrong", "s3-standard-wrong", nil, 1, false, CryptUnreadable, "undecryptable"},
		{"sftp-standard-wrong", "sftp-standard-wrong", nil, 1, false, CryptUnreadable, "undecryptable"},
		// Walked folder by folder, garbled folder names are missing.
		{"sftp-obfuscate-wrong", "sftp-obfuscate-wrong", nil, 3, false, CryptUnreadable, "directory not found"},
		// Nothing stored yet: the folder (or the bucket) does not exist.
		{"sftp-missing-folder", "sftp-missing-folder", nil, 3, false, CryptNothing, ""},
		{"s3-missing-bucket", "s3-missing-bucket", nil, 3, false, CryptNothing, ""},
		// No file with data: nothing to decrypt.
		{"nothing", "", nil, 0, false, CryptNothing, ""},
		// Anything else is the caller's to classify.
		{"failed", "", nil, 5, false, CryptFailed, ""},
	} {
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
			for _, o := range tt.stdout {
				c.Stdout(proc.Line{Data: []byte(o)})
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

// TestCryptCheckSignals: which lines are signs of a wrong password. A file
// that fails for another reason, or a missing root, is not; the first
// decrypted byte wins over any of them.
func TestCryptCheckSignals(t *testing.T) {
	t.Parallel()
	line := func(level, msg, object string) proc.Line {
		return proc.Line{Data: []byte(`{"level":"` + level + `","msg":"` + msg + `","object":"` + object + `"}`)}
	}
	for _, tt := range []struct {
		name string
		l    proc.Line
		want CryptVerdict
	}{
		{"bad block on open", line("error", "Failed to open: failed to authenticate decrypted block - bad password?", "a"), CryptUnreadable},
		{"undecryptable name", line("error", "error listing: there were 1 undecryptable name errors", ""), CryptUnreadable},
		{"garbled folder missing", line("error", "error listing: directory not found", "x/y"), CryptUnreadable},
		{"root missing", line("error", "error listing: directory not found", ""), CryptFailed},
		{"network", line("error", "Failed to open: connection reset by peer", "a"), CryptFailed},
		{"not an encrypted file", line("error", "Failed to send to output: not an encrypted file - bad magic string", "a"), CryptFailed},
		{"a file named like a sign", line("error", "Failed to open: permission denied", "undecryptable bad password"), CryptFailed},
		{"plain text", proc.Line{Data: []byte("2026/11/20 ERROR : bad password")}, CryptFailed},
		{"truncated", proc.Line{Data: []byte(`{"level":"error","msg":"Failed to open: bad password"}`), Truncated: true}, CryptFailed},
	} {
		c := NewCryptCheck(NewTracker(nil, nil), func() {})
		c.Stderr(tt.l)
		// exit 1, an error: what the line says decides.
		if got := c.Verdict(1, errors.New("exit 1")); got != tt.want {
			t.Errorf("%s: Verdict = %v, want %v", tt.name, got, tt.want)
		}
		c.Stdout(proc.Line{Data: []byte("x")})
		if got := c.Verdict(1, errors.New("exit 1")); got != CryptReadable {
			t.Errorf("%s, then a decrypted byte: Verdict = %v, want readable", tt.name, got)
		}
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
