package restic

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Exit codes of restic 0.19 (see "restic help backup", EXIT STATUS, and the
// documentation's "Exit codes" table).
const (
	exitOK              = 0
	exitFatal           = 1   // command failed, e.g. no snapshot created
	exitRuntime         = 2   // Go runtime error
	exitIncomplete      = 3   // backup: some source data could not be read
	exitNoRepository    = 10  // repository does not exist
	exitLocked          = 11  // failed to lock the repository
	exitWrongPassword   = 12  // wrong password
	exitInterrupted     = 130 // interrupted by SIGINT/SIGTERM
	maxFailureDetail    = 1000
	maxTailLines        = 4
	maxTailLineBytes    = 400
	redactedPlaceholder = "[redacted]"
)

// ExitIncomplete is the exit code restic uses for a backup that created a
// snapshot but could not read some source data. A backup runner result with
// this ExitCode succeeded with warnings; Stats.Errors counts the unreadable
// items.
const ExitIncomplete = exitIncomplete

// Sentinel errors classifying restic failures. Use errors.Is on the error
// returned by a runner. They all report syncrv1.JobErrorCode EXECUTION_FAILED
// until the wire contract grows dedicated codes (see jobErrorCode).
var (
	// ErrRepositoryNotFound: the repository does not exist (exit code 10).
	ErrRepositoryNotFound = errors.New("repository does not exist")
	// ErrRepositoryLocked: the repository is locked by another operation
	// (exit code 11). The controller serializes jobs per repository, so this
	// means a stale lock or a client outside Syncr.
	ErrRepositoryLocked = errors.New("repository is locked")
	// ErrWrongPassword: the repository password is wrong (exit code 12).
	ErrWrongPassword = errors.New("wrong repository password")
	// ErrInterrupted: restic was interrupted without the job being cancelled
	// (exit code 130).
	ErrInterrupted = errors.New("restic was interrupted")
)

// ExitError reports that restic exited unsuccessfully. Message is restic's
// own explanation, scrubbed of secrets and truncated.
type ExitError struct {
	// Verb is the restic command that failed ("backup", "check", ...).
	Verb string
	// ExitCode is restic's exit code, or -1 if it was killed by a signal.
	ExitCode int
	// Message is a short, scrubbed explanation from restic.
	Message string
}

// Error implements error. The text is shown to users.
func (e *ExitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "restic %s: %s", e.Verb, headline(e.ExitCode))
	if e.ExitCode >= 0 {
		fmt.Fprintf(&b, " (exit code %d)", e.ExitCode)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap returns the sentinel for the exit code (ErrRepositoryNotFound,
// ErrRepositoryLocked, ErrWrongPassword, ErrInterrupted), or nil.
func (e *ExitError) Unwrap() error {
	switch e.ExitCode {
	case exitNoRepository:
		return ErrRepositoryNotFound
	case exitLocked:
		return ErrRepositoryLocked
	case exitWrongPassword:
		return ErrWrongPassword
	case exitInterrupted:
		return ErrInterrupted
	}
	return nil
}

// headline describes an exit code in one phrase.
func headline(code int) string {
	switch code {
	case exitFatal:
		return "command failed"
	case exitRuntime:
		return "internal error in restic"
	case exitIncomplete:
		return "some source data could not be read"
	case exitNoRepository:
		return "repository does not exist"
	case exitLocked:
		return "repository is locked by another operation"
	case exitWrongPassword:
		return "wrong repository password"
	case exitInterrupted:
		return "interrupted"
	case -1:
		return "terminated by a signal"
	}
	return "command failed"
}

// jobErrorCode maps a restic failure to the wire error code. The contract has
// no dedicated codes for a missing repository, a lock conflict or a wrong
// password yet, so all of them are EXECUTION_FAILED; callers that need the
// distinction use errors.Is with the Err* sentinels. When the proto grows
// REPOSITORY_NOT_FOUND, REPOSITORY_LOCKED and WRONG_PASSWORD, this is the
// only place to change.
func jobErrorCode(*ExitError) syncrv1.JobErrorCode {
	return syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
}

// scrubbedError is an error whose text has been scrubbed of secrets. It
// unwraps to the original so errors.Is/As (and runner.CodeOf) still work.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubber removes secret values from text.
type scrubber struct {
	r *strings.Replacer
}

// newScrubber returns a scrubber for secrets; empty values are ignored.
// Longer secrets are replaced first so one that contains another is removed
// as a whole.
func newScrubber(secrets []string) *scrubber {
	uniq := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		if s != "" {
			uniq[s] = struct{}{}
		}
	}
	list := make([]string, 0, len(uniq))
	for s := range uniq {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	pairs := make([]string, 0, 2*len(list))
	for _, s := range list {
		pairs = append(pairs, s, redactedPlaceholder)
	}
	return &scrubber{r: strings.NewReplacer(pairs...)}
}

// String returns s without secrets.
func (sc *scrubber) String(s string) string { return sc.r.Replace(s) }

// Error returns err with a scrubbed message, or err itself when scrubbing
// changes nothing. It returns nil for nil.
func (sc *scrubber) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := sc.r.Replace(msg)
	if clean == msg {
		return err
	}
	return &scrubbedError{msg: clean, err: err}
}

// secretsOf lists the secret values of a job spec: the repository password
// and the secret options of every remote.
func secretsOf(spec *syncrv1.JobSpec) []string {
	var out []string
	for _, repo := range []interface{ GetPassword() string }{
		spec.GetResticInit().GetRepository(),
		spec.GetResticBackup().GetRepository(),
		spec.GetResticForget().GetRepository(),
		spec.GetResticPrune().GetRepository(),
		spec.GetResticCheck().GetRepository(),
		spec.GetResticRestore().GetRepository(),
		spec.GetResticSnapshots().GetRepository(),
	} {
		out = append(out, repo.GetPassword())
	}
	for _, r := range spec.GetRemotes() {
		for _, v := range r.GetSecrets() {
			out = append(out, v)
		}
	}
	return out
}

// failure builds the runner error for a restic exit.
func failure(verb string, code int, message string) error {
	e := &ExitError{Verb: verb, ExitCode: code, Message: message}
	return runner.Fail(jobErrorCode(e), e)
}

// clip shortens s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
