package link

import (
	"errors"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func TestActiveAuthorization(t *testing.T) {
	t.Parallel()
	const id, secret = "0195f0a4-3b7c-7d1e-9f00-123456789abc", "syncr_ws_a.b-c_d"
	h := ActiveAuthorization(id, secret)
	if h != "Bearer "+id+"."+secret {
		t.Fatalf("header = %q", h)
	}
	gotID, gotSecret, err := ParseActiveAuthorization(h)
	if err != nil || gotID != id || gotSecret != secret {
		t.Errorf("parse = (%q, %q, %v)", gotID, gotSecret, err)
	}
	for _, bad := range []string{"", "Bearer ", "Bearer id", "Bearer .secret", "Bearer id.", "Basic id.secret", "bearer id.secret"} {
		if _, _, err := ParseActiveAuthorization(bad); !errors.Is(err, ErrBadAuthorization) {
			t.Errorf("ParseActiveAuthorization(%q) err = %v", bad, err)
		}
	}
}

func TestPassiveAuthorization(t *testing.T) {
	t.Parallel()
	got, err := ParsePassiveAuthorization(PassiveAuthorization("s3cret.x"))
	if err != nil || got != "s3cret.x" {
		t.Errorf("round trip = (%q, %v)", got, err)
	}
	for _, bad := range []string{"", "Bearer ", "s3cret"} {
		if _, err := ParsePassiveAuthorization(bad); !errors.Is(err, ErrBadAuthorization) {
			t.Errorf("ParsePassiveAuthorization(%q) err = %v", bad, err)
		}
	}
}

func TestErrorCodeNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code syncrv1.JobErrorCode
		name string
	}{
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED, "unspecified"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid_spec"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED, "cancelled"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_DISPATCH_TIMEOUT, "dispatch_timeout"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_REMOVED, "worker_removed"},
		// Milestone 5: the controller's job error codes store these names.
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS, "already_exists"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY, "busy"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT, "timeout"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_MISMATCH, "repository_mismatch"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_UNAVAILABLE, "repository_unavailable"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_WORKER_OUTDATED, "worker_outdated"},
		// Milestone 6: set by workers (jobs.CodeTimeLimit).
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_TIME_LIMIT, "time_limit"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_HOOK_FAILED, "hook_failed"},
		// Two-way sync (rclone_bisync): set by workers.
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, "resync_required"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT, "safety_abort"},
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, "side_unavailable"},
		// Backups of a remote (restic_backup with a remote_source): set by
		// workers.
		{syncrv1.JobErrorCode_JOB_ERROR_CODE_SOURCE_EMPTY, "source_empty"},
		{syncrv1.JobErrorCode(999), "unspecified"},
	}
	for _, tt := range tests {
		if got := ErrorCodeName(tt.code); got != tt.name {
			t.Errorf("ErrorCodeName(%v) = %q, want %q", tt.code, got, tt.name)
		}
	}
	// Every defined code round-trips.
	for v := range syncrv1.JobErrorCode_name {
		c := syncrv1.JobErrorCode(v)
		if got := ParseErrorCode(ErrorCodeName(c)); got != c {
			t.Errorf("round trip of %v = %v", c, got)
		}
	}
	for _, bad := range []string{"", "nope", "JOB_ERROR_CODE_INTERNAL"} {
		if got := ParseErrorCode(bad); got != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
			t.Errorf("ParseErrorCode(%q) = %v", bad, got)
		}
	}
}
