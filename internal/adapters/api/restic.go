package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// resticHandler serves ResticService over the restic use cases. Its
// procedures are not implemented yet: every call answers Unimplemented,
// after the guard authenticated it.
type resticHandler struct {
	syncrv1connect.UnimplementedResticServiceHandler

	restic ResticService
	errs   errorMapper
}
