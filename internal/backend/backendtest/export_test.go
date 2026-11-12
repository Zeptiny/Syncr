package backendtest

import "github.com/Zeptiny/Syncr/internal/backend"

// Check runs the suite's checks on typ with r collecting the failures, for
// the suite's own tests.
func Check(r interface {
	Helper()
	Errorf(format string, args ...any)
}, typ backend.Type, opts ...Option) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	checkDescriptor(r, typ)
	checkValidate(r, typ)
	checkInvalid(r, typ)
	checkToRclone(r, typ, o)
}
