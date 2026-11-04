package rclone

import syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"

// exitClass describes an rclone exit code.
type exitClass struct {
	success bool
	code    syncrv1.JobErrorCode
	summary string
}

// classifyExit maps rclone's documented exit codes
// (https://rclone.org/docs/#exit-code, lib/exitcode) to job outcomes.
func classifyExit(exit int) exitClass {
	fail := func(code syncrv1.JobErrorCode, summary string) exitClass {
		return exitClass{code: code, summary: summary}
	}
	switch exit {
	case 0:
		return exitClass{success: true}
	case 1:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "error")
	case 2:
		// Syncr builds the arguments, so this means a value slipped past
		// validation.
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "rclone rejected the arguments")
	case 3:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found")
	case 4:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "file not found")
	case 5:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, "temporary error, retrying may help")
	case 6:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "errors that retrying will not fix")
	case 7:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "fatal error, retrying will not help")
	case 8:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED, "transfer limit (max_transfer_bytes) reached")
	case 9:
		// "Operation successful, but no files transferred" is only reported
		// with --error-on-no-transfer, which Syncr does not pass.
		return exitClass{success: true}
	case 10:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED, "duration limit (max_duration) reached")
	case -1:
		return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "rclone was terminated by a signal")
	}
	return fail(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "unexpected exit code")
}
