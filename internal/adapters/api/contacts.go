package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// contactHandler implements syncr.v1.ContactService over the contacts and
// notifications use cases (Deps.Contacts, Deps.Deliveries) and the channel
// descriptors (Deps.Channels). The contracts step mounts it with every
// procedure Unimplemented; the contacts track fills it in.
type contactHandler struct {
	syncrv1connect.UnimplementedContactServiceHandler

	contacts   ContactService
	deliveries DeliveryService
	channels   *notify.Registry
	errs       errorMapper
}

var _ syncrv1connect.ContactServiceHandler = (*contactHandler)(nil)
