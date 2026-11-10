package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// probeHandler serves ProbeService over the probe use cases. Its
// procedures are not implemented yet: every call answers Unimplemented,
// after the guard authenticated it.
type probeHandler struct {
	syncrv1connect.UnimplementedProbeServiceHandler

	probes ProbeService
	errs   errorMapper
}
