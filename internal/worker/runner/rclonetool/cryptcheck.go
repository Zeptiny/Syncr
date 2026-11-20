package rclonetool

import (
	"bytes"
	"encoding/json"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// The password check of a crypt remote.
//
// Reading a file is the only check of a crypt remote's password that holds
// in every file-name mode (rclone 1.75.2, with strict_names, which the
// worker always sets):
//
//   - standard names: a listing fails on the names it cannot decrypt;
//   - obfuscated names: names list, garbled, without an error. A garbled
//     folder name encrypts to another name, so a walk folder by folder
//     finds that folder missing (SFTP) or, on a bucket (S3, B2), empty,
//     and a sync from the remote's root empties its destination; a sync
//     into it deletes the files at its root;
//   - names not encrypted: names list as they are.
//
// In every mode a file's data fails to decrypt: "failed to authenticate
// decrypted block - bad password?". So the check reads the first byte of
// the remote's files with data, from the remote's root, and stops at the
// first one that decrypts. rclone lists the root recursively with the
// backend's recursive listing where it has one (S3, B2; rclone uses it
// whenever no folder filter is set, and it never encrypts a decrypted name
// again), and folder by folder elsewhere, where a garbled folder is
// missing. Opening a file by its path would not do: a garbled path
// encrypts to another one, which a bucket lists as an empty folder.

// cryptCheckReads is how many files the check fails to decrypt before it
// stops rclone: more than one, so that a damaged file does not fail every
// job of a remote whose password is right.
const cryptCheckReads = 3

// CryptCheckArgs returns rclone's arguments of the password check of the
// crypt remote alias: "rclone cat" of the first byte of each file with
// data (an empty file has no block to authenticate), from the remote's
// root, with JSON log lines. flags (such as "--config=") follow the
// command.
func CryptCheckArgs(alias string, flags ...string) []string {
	args := []string{"cat"}
	args = append(args, flags...)
	return append(args, "--use-json-log", "--log-level=NOTICE", "--count=1", "--min-size=1B", "--", alias+":")
}

// CryptVerdict is the outcome of a password check.
type CryptVerdict int

const (
	// CryptFailed means that the check ended without a verdict; rclone's
	// exit and messages say why.
	CryptFailed CryptVerdict = iota
	// CryptReadable means that a file's first block decrypted (it is
	// authenticated), so the password is right.
	CryptReadable
	// CryptNothing means that the remote holds no file with data, or that
	// its folder does not exist yet: there is nothing to check the
	// password with.
	CryptNothing
	// CryptUnreadable means that nothing decrypted, and a file's data or a
	// name did not, or a folder that the listing itself found was missing
	// (its name decrypted to another one): the password is wrong, or the
	// folder holds files the remote did not write.
	CryptUnreadable
)

// CryptCheck reads the output of a password check (CryptCheckArgs). Stdout
// must receive rclone's standard output in chunks (proc.Spec.StdoutChunks)
// and Stderr its standard error. It calls stop once it knows the verdict:
// at the first decrypted byte, or after cryptCheckReads files that failed
// to decrypt. Its methods are called from proc callbacks, which run
// sequentially on the Run goroutine.
type CryptCheck struct {
	t    *Tracker
	stop func()
	// read: a decrypted byte arrived.
	read bool
	// unreadable: the files whose data failed to decrypt.
	unreadable int
	// wrong: a sign of a wrong password (data or a name that did not
	// decrypt, a folder of the listing that was missing).
	wrong bool
}

// NewCryptCheck returns a CryptCheck that keeps rclone's error messages in
// t (scrubbed, for the failure message) and calls stop once it knows the
// verdict.
func NewCryptCheck(t *Tracker, stop func()) *CryptCheck {
	return &CryptCheck{t: t, stop: stop}
}

// Stdout handles a chunk of standard output: decrypted data.
func (c *CryptCheck) Stdout(l proc.Line) {
	if len(l.Data) > 0 && !c.read {
		c.read = true
		c.stop()
	}
}

// Stderr handles a line of standard error: rclone's JSON log.
func (c *CryptCheck) Stderr(l proc.Line) {
	c.t.Line(l)
	data := bytes.TrimSpace(l.Data)
	var ll LogLine
	if len(data) == 0 || data[0] != '{' || l.Truncated || json.Unmarshal(data, &ll) != nil {
		return
	}
	msg := ll.Msg
	switch {
	case strings.Contains(msg, "undecryptable"):
		// "there were N undecryptable name errors" (strict_names), in a
		// folder's listing or in rclone's final notice.
		c.wrong = true
	case strings.HasPrefix(msg, "error listing: ") && ll.Object != "" && strings.Contains(msg, "directory not found"):
		// A folder below the root that the listing found: its name
		// decrypted to another one. A missing root ("object" empty) only
		// means that nothing is stored yet.
		c.wrong = true
	case (strings.HasPrefix(msg, "Failed to open: ") || strings.HasPrefix(msg, "Failed to send to output: ")) &&
		strings.Contains(msg, "bad password"):
		c.wrong = true
		c.unreadable++
		if c.unreadable == cryptCheckReads {
			c.stop()
		}
	}
	if strings.EqualFold(ll.Level, "notice") && strings.HasPrefix(msg, "Failed to cat") {
		// rclone's final notice names the reason when no error line does
		// (a recursive listing that failed).
		c.t.Note(msg)
	}
}

// Verdict returns the check's outcome from rclone's exit code and the
// supervisor's error. A check stopped by the caller's context (not by the
// check itself) has no verdict; callers test their context first.
func (c *CryptCheck) Verdict(exitCode int, err error) CryptVerdict {
	switch {
	case c.read:
		return CryptReadable
	case c.wrong:
		return CryptUnreadable
	case err == nil:
		return CryptNothing
	case exitCode == 3:
		// "directory not found" without any other sign: the remote's own
		// folder (or the bucket) does not exist, so nothing is stored.
		return CryptNothing
	}
	return CryptFailed
}

// CryptUnreadableError is the error of a check whose verdict is
// CryptUnreadable, for the remote that what names ("source", "path2",
// ...; empty: no name): EXECUTION_FAILED with rclone's messages
// (diagnostics, scrubbed).
func CryptUnreadableError(what string, diagnostics []string) error {
	var b strings.Builder
	if what != "" {
		b.WriteString(what)
		b.WriteString(": ")
	}
	b.WriteString("the encrypted remote cannot decrypt the files stored in its folder, so its password is wrong ")
	b.WriteString("(or the folder holds files it did not write); nothing was read or written")
	if len(diagnostics) > 0 {
		b.WriteString(": rclone: ")
		b.WriteString(strings.Join(diagnostics, "; "))
	}
	return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "%s", b.String())
}
