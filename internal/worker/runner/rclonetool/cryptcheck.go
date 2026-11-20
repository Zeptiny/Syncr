package rclonetool

import (
	"bytes"
	"encoding/json"
	"maps"
	"regexp"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// The password check of a crypt remote.
//
// Reading a file is the only check of a crypt remote's password that holds
// in every file-name mode (rclone 1.75.2):
//
//   - standard names: no name decrypts;
//   - obfuscated names: names list, garbled, without an error. A garbled
//     folder name encrypts to another name, so a walk folder by folder
//     finds that folder missing (SFTP, WebDAV) or, on a bucket (S3, B2),
//     empty, and a sync from the remote's root empties its destination; a
//     sync into it deletes the files at its root;
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
//
// The check runs with strict_names off (CryptCheckEnv), while
// jobs keep it on: a name that does not decrypt is then skipped with a
// notice ("Skipping undecryptable file name"), which the check counts as a
// sign, instead of failing the whole listing (on a bucket, a page of up to
// 1,000 objects), so one foreign file does not hide the files that
// decrypt.

// cryptCheckSigns is how many signs of a wrong password (a file that does
// not decrypt, a name that does not, a listed folder that is missing) the
// check accepts before it stops rclone without a decrypted byte: more than
// one, so that one damaged or foreign file does not fail every job of a
// remote whose password is right.
const cryptCheckSigns = 3

// CryptCheckArgs returns rclone's arguments of the password check of the
// crypt remote alias: "rclone cat" of the first byte of each file with
// data (an empty file has no block to authenticate), from the remote's
// root, with JSON log lines. flags (such as "--config=") follow the
// command. The check's environment is CryptCheckEnv's.
//
// --min-size=1B also skips a file whose size the storage does not report:
// crypt then reports it as 0 bytes (rclone 1.75.2, Object.Size). Such a
// file counts as empty, which only matters on a storage that reports no
// sizes at all (a remote of only such files has nothing to check).
func CryptCheckArgs(alias string, flags ...string) []string {
	args := []string{"cat"}
	args = append(args, flags...)
	return append(args, "--use-json-log", "--log-level=NOTICE", "--count=1", "--min-size=1B", "--", alias+":")
}

// CryptCheckEnv returns a copy of vars, the RCLONE_CONFIG_* variables of a
// job's remotes, in which the crypt remote alias has strict_names off: the
// environment of its password check. rclone then skips a name it cannot
// decrypt with a notice ("Skipping undecryptable file name"), which the
// check counts, instead of failing the listing of the folder (on a bucket,
// of a page of up to 1,000 objects) on it. Jobs keep the "true" the worker
// sets. The variable is named as rcloneenv.EnvName names it (aliases are
// lowercase letters, digits and "_"); the rclone runner's tests check
// that it replaces the worker's.
func CryptCheckEnv(vars map[string]string, alias string) map[string]string {
	out := maps.Clone(vars)
	if out == nil {
		out = map[string]string{}
	}
	out["RCLONE_CONFIG_"+strings.ToUpper(alias)+"_STRICT_NAMES"] = "false"
	return out
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

// The messages of rclone 1.75.2 that the check reads (msg of a JSON log
// line; the object is in its own field).
const (
	// A file's data that does not decrypt, after "Failed to open: " or
	// "Failed to send to output: ".
	msgBadPassword = "failed to authenticate decrypted block - bad password?"
	// A folder that a walk (SFTP, WebDAV...) cannot list. With an object,
	// a folder of the listing: its name decrypted to another one. With an
	// empty object, the remote's own folder: nothing is stored yet.
	msgDirNotFound = "error listing: directory not found"
	// The final notice when a bucket does not exist: nothing is stored yet.
	msgNoBucket = "Failed to cat: directory not found"
)

// undecryptableRe matches what crypt says of a name it cannot decrypt:
// the notice with strict_names off ("Skipping undecryptable file name: ",
// "... dir name: "), and the error with it on ("there were N undecryptable
// name errors. first error: ...", after "error listing: " or rclone's final
// notice). Both are anchored, so a file name in the message never matches.
var undecryptableRe = regexp.MustCompile(`^(Skipping undecryptable (file|dir) name: |([^:]+: )+there were [0-9]+ undecryptable name errors\. )`)

// CryptCheck reads the output of a password check (CryptCheckArgs). Stdout
// must receive rclone's standard output in chunks (proc.Spec.StdoutChunks)
// and Stderr its standard error. It calls stop once it knows the verdict:
// at the first decrypted byte, or after cryptCheckSigns signs of a wrong
// password. Its methods are called from proc callbacks, which run
// sequentially on the Run goroutine.
type CryptCheck struct {
	t    *Tracker
	stop func()
	// stopped: stop was called.
	stopped bool
	// read: a decrypted byte arrived.
	read bool
	// signs: the signs of a wrong password so far (data or a name that
	// did not decrypt, a folder of the listing that was missing).
	signs int
	// noRoot: rclone said that the remote's own folder, or its bucket,
	// does not exist.
	noRoot bool
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
		c.halt()
	}
}

// halt stops rclone, once.
func (c *CryptCheck) halt() {
	if !c.stopped {
		c.stopped = true
		c.stop()
	}
}

// cryptLogLine is the part of rclone's JSON log line that the check reads.
// Object is a pointer: an empty object (the remote's root) is not a
// missing one.
type cryptLogLine struct {
	Level  string  `json:"level"`
	Msg    string  `json:"msg"`
	Object *string `json:"object"`
}

// Stderr handles a line of standard error: rclone's JSON log.
func (c *CryptCheck) Stderr(l proc.Line) {
	c.t.Line(l)
	data := bytes.TrimSpace(l.Data)
	var ll cryptLogLine
	if len(data) == 0 || data[0] != '{' || l.Truncated || json.Unmarshal(data, &ll) != nil {
		return
	}
	msg := ll.Msg
	object := ""
	if ll.Object != nil {
		object = *ll.Object
	}
	sign := false
	switch {
	case undecryptableRe.MatchString(msg):
		sign = true
		if strings.HasPrefix(msg, "Skipping ") {
			// A notice, which the tracker does not keep: it names the
			// file, which the failure message should show.
			c.t.Note(object + ": " + msg)
		}
	case msg == msgDirNotFound && ll.Object != nil:
		if object == "" {
			c.noRoot = true
		} else {
			// A folder below the root that the listing found: its name
			// decrypted to another one.
			sign = true
		}
	case msg == "Failed to open: "+msgBadPassword || msg == "Failed to send to output: "+msgBadPassword:
		sign = true
	case strings.EqualFold(ll.Level, "notice") && msg == msgNoBucket:
		c.noRoot = true
	}
	if strings.EqualFold(ll.Level, "notice") && strings.HasPrefix(msg, "Failed to cat") {
		// rclone's final notice names the reason when no error line does
		// (a recursive listing that failed).
		c.t.Note(msg)
	}
	if sign && !c.read {
		c.signs++
		if c.signs >= cryptCheckSigns {
			c.halt()
		}
	}
}

// Verdict returns the check's outcome from rclone's exit code and the
// supervisor's error: readable after a decrypted byte, whatever else
// happened; otherwise unreadable after any sign of a wrong password;
// otherwise nothing to check after a run without an error, or one that
// only found the remote's folder (or bucket) missing (exit 3). Anything
// else has no verdict, so that an unknown message never lets a job run.
// A check stopped by the caller's context (not by the check itself) has
// no verdict either; callers test their context first.
func (c *CryptCheck) Verdict(exitCode int, err error) CryptVerdict {
	switch {
	case c.read:
		return CryptReadable
	case c.signs > 0:
		return CryptUnreadable
	case err == nil:
		return CryptNothing
	case exitCode == 3 && c.noRoot:
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
