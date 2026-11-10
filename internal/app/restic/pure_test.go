package restic

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// fieldOf returns the single field of a validation error, or "".
func fieldOf(err error) string {
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || len(verr.Fields) != 1 {
		return ""
	}
	return verr.Fields[0].Field
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode Mode
		pw   string
		ok   bool
	}{
		{"init, long enough", ModeInit, "12345678", true},
		{"init, eight characters of several bytes", ModeInit, "éééééééé", true},
		{"init, too short", ModeInit, "1234567", false},
		{"connect accepts a short one", ModeConnect, "x", true},
		{"empty", ModeConnect, "", false},
		{"NUL", ModeConnect, "abc\x00defgh", false},
		{"invalid UTF-8", ModeConnect, "abc\xffdefgh", false},
		{"at the limit", ModeInit, strings.Repeat("a", MaxPasswordLen), true},
		{"over the limit", ModeConnect, strings.Repeat("a", MaxPasswordLen+1), false},
	}
	for _, tt := range tests {
		err := checkPassword(tt.mode, tt.pw)
		if (err == nil) != tt.ok {
			t.Errorf("%s: checkPassword = %v, want ok %v", tt.name, err, tt.ok)
		}
		if err != nil && (fieldOf(err) != "password" || (len(tt.pw) > 3 && strings.Contains(err.Error(), tt.pw))) {
			t.Errorf("%s: error %v is not a password field error, or contains the password", tt.name, err)
		}
	}
}

func TestVerifyErrorFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode    Mode
		code    string
		field   string
		message string
	}{
		{ModeConnect, "wrong_password", "password", "is not this repository's password"},
		{ModeInit, "wrong_password", "password", "is not this repository's password"},
		{ModeConnect, "repository_not_found", "path", "no restic repository at this location"},
		{ModeInit, "already_exists", "mode", "a repository already exists here; choose Connect"},
		{ModeConnect, "path_not_allowed", "worker_id", "the location is outside the worker's allowed folders"},
		{ModeInit, "path_not_allowed", "worker_id", "the location is outside the worker's allowed folders"},
		// Not field errors.
		{ModeInit, "repository_not_found", "", ""},
		{ModeConnect, "already_exists", "", ""},
		{ModeConnect, "repository_locked", "", ""},
		{ModeConnect, "execution_failed", "", ""},
		{ModeConnect, "not_found", "", ""},
	}
	for _, tt := range tests {
		err := verifyErrorFields(tt.mode, &ProbeError{Code: tt.code, Message: "what restic said"})
		var verr *domain.ValidationError
		if tt.field == "" {
			if !errors.Is(err, domain.ErrFailedPrecondition) || errors.As(err, &verr) || !strings.Contains(err.Error(), "what restic said") {
				t.Errorf("%s/%s: %v, want a failed precondition with the message", tt.mode, tt.code, err)
			}
			continue
		}
		if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != tt.field || verr.Fields[0].Message != tt.message {
			t.Errorf("%s/%s: %v, want %s: %s", tt.mode, tt.code, err, tt.field, tt.message)
		}
	}
	if err := verifyErrorFields(ModeInit, &ProbeError{}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("an empty probe error: %v", err)
	}
}

func TestVerifyErrorScrubs(t *testing.T) {
	t.Parallel()
	const pw = "sentinel-password"
	pe := &ProbeError{Code: "execution_failed", Message: "Fatal: bad key " + pw}
	wrapped := errors.Join(domain.ErrFailedPrecondition, pe)
	if err := verifyError(ModeConnect, wrapped, pw); strings.Contains(err.Error(), pw) || !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("verifyError(probe error) = %v", err)
	}
	plain := errors.Join(domain.ErrUnavailable, errors.New("worker said "+pw))
	if err := verifyError(ModeConnect, plain, pw); strings.Contains(err.Error(), pw) || !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("verifyError(other error) = %v", err)
	}
	clean := errors.New("nothing secret")
	if err := verifyError(ModeConnect, clean, pw); !errors.Is(err, clean) {
		t.Errorf("verifyError(clean) = %v, want it unchanged", err)
	}
}

func TestStale(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		l    SnapshotList
		want bool
	}{
		{"never listed", SnapshotList{}, true},
		{"never listed, with a change", SnapshotList{LastChangeAt: at}, true},
		{"listed, no change", SnapshotList{RefreshedAt: at}, false},
		{"change before the listing", SnapshotList{RefreshedAt: at, LastChangeAt: at.Add(-time.Second)}, false},
		{"change at the listing", SnapshotList{RefreshedAt: at, LastChangeAt: at}, false},
		{"change after the listing", SnapshotList{RefreshedAt: at, LastChangeAt: at.Add(time.Second)}, true},
	}
	for _, tt := range tests {
		if got := stale(tt.l); got != tt.want {
			t.Errorf("%s: stale = %v, want %v", tt.name, got, tt.want)
		}
		st := RepoStats{SnapshotsRefreshedAt: tt.l.RefreshedAt, LastChangeAt: tt.l.LastChangeAt}
		if got := st.SnapshotsStale(); got != tt.want {
			t.Errorf("%s: SnapshotsStale = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCheckTreePath(t *testing.T) {
	t.Parallel()
	for p, ok := range map[string]bool{
		"/": true, "/srv/photos": true, "/C/Users/ada": true, "/a b/[x]*?/ü": true, "/a/..b/c..": true,
		"": false, "srv": false, "/srv/../etc": false, "/..": false, "/a\nb": false, "/a\x00": false,
		"/" + strings.Repeat("a", 4*MaxPathLen): false,
	} {
		if msg := checkTreePath(p); (msg == "") != ok {
			t.Errorf("checkTreePath(%q) = %q, want ok %v", p, msg, ok)
		}
	}
}

func TestDeletionOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		p    PendingDeletion
		ok   bool
		want string
	}{
		{PendingDeletion{JobStatus: domain.JobSucceeded}, true, ""},
		{PendingDeletion{JobStatus: domain.JobFailed, JobErrorCode: "repository_locked", JobErrorMessage: "another client holds a lock"}, false,
			"deleting the data failed: repository_locked: another client holds a lock"},
		{PendingDeletion{JobStatus: domain.JobInterrupted, JobErrorCode: "worker_lost"}, false, "deleting the data failed: worker_lost"},
		{PendingDeletion{JobStatus: domain.JobFailed}, false, "deleting the data failed: the job was failed"},
		{PendingDeletion{JobStatus: domain.JobCancelled, JobErrorCode: "cancelled"}, false, "the deletion was cancelled; some repository files may already be gone"},
		{PendingDeletion{}, false, "the deletion job was never created; try again"},
	}
	for _, tt := range tests {
		if got := deletionOutcome(tt.p); got.Succeeded != tt.ok || got.Error != tt.want || got.Cancelled != (tt.p.JobStatus == domain.JobCancelled) {
			t.Errorf("deletionOutcome(%+v) = %+v", tt.p, got)
		}
	}
	long := deletionOutcome(PendingDeletion{JobStatus: domain.JobFailed, JobErrorCode: "x", JobErrorMessage: strings.Repeat("é", 2000)})
	if n := len([]rune(long.Error)); n != MaxDeletionErrorLen {
		t.Errorf("a long error is %d runes, want %d", n, MaxDeletionErrorLen)
	}
}
