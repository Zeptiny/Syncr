package restic

import (
	"errors"
	"fmt"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Exit codes of restic 0.19 (see "restic help backup", EXIT STATUS, and the
// documentation's "Exit codes" table).
const (
	exitOK            = 0
	exitFatal         = 1   // command failed, e.g. no snapshot created
	exitRuntime       = 2   // Go runtime error
	exitIncomplete    = 3   // backup: some source data could not be read
	exitNoRepository  = 10  // repository does not exist
	exitLocked        = 11  // failed to lock the repository
	exitWrongPassword = 12  // wrong password
	exitInterrupted   = 130 // interrupted by SIGINT/SIGTERM
	maxFailureDetail  = 1000
	maxTailLines      = 4
	maxTailLineBytes  = 400
	maxItemErrors     = runner.MaxWarnings - 1 // plus the summary warning
)

// ExitIncomplete is the exit code restic uses for a backup that created a
// snapshot but could not read some source data. A backup runner result with
// this ExitCode succeeded with warnings; Stats.Errors counts the unreadable
// items.
const ExitIncomplete = exitIncomplete

// Sentinel errors classifying restic failures. Use errors.Is on the error
// returned by a runner. The first three also have dedicated wire codes
// (REPOSITORY_NOT_FOUND, REPOSITORY_LOCKED, WRONG_PASSWORD; see jobErrorCode).
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

// jobErrorCode maps a restic failure to the wire error code: the dedicated
// repository codes for exit codes 10, 11 and 12, EXECUTION_FAILED otherwise.
func jobErrorCode(e *ExitError) syncrv1.JobErrorCode {
	switch e.ExitCode {
	case exitNoRepository:
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND
	case exitLocked:
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_LOCKED
	case exitWrongPassword:
		return syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD
	}
	return syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED
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
