package link

import (
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// errorCodePrefix is the common prefix of JobErrorCode value names.
const errorCodePrefix = "JOB_ERROR_CODE_"

// ErrorCodeName returns the storage and display name of a job error code:
// its enum value name without the prefix, in lower case (e.g.
// JOB_ERROR_CODE_INVALID_SPEC -> "invalid_spec"). Unknown values and
// UNSPECIFIED return "unspecified".
func ErrorCodeName(c syncrv1.JobErrorCode) string {
	name, ok := syncrv1.JobErrorCode_name[int32(c)]
	if !ok || c == syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED {
		return "unspecified"
	}
	return strings.ToLower(strings.TrimPrefix(name, errorCodePrefix))
}

// ParseErrorCode is the inverse of ErrorCodeName. Unknown names return
// JOB_ERROR_CODE_UNSPECIFIED.
func ParseErrorCode(name string) syncrv1.JobErrorCode {
	if v, ok := syncrv1.JobErrorCode_value[errorCodePrefix+strings.ToUpper(name)]; ok && name != "" {
		return syncrv1.JobErrorCode(v)
	}
	return syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSPECIFIED
}
