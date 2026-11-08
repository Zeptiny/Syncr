package api

import (
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// channelHandler implements syncr.v1.ChannelService: the descriptors of
// Deps.Channels, static for a controller build (served with fieldToProto,
// like BackendService). The contracts step mounts it Unimplemented; the
// contacts track fills it in.
type channelHandler struct {
	syncrv1connect.UnimplementedChannelServiceHandler

	channels *notify.Registry
}

var _ syncrv1connect.ChannelServiceHandler = (*channelHandler)(nil)
