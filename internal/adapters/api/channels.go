package api

import (
	"context"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// channelHandler implements syncr.v1.ChannelService: the descriptors of
// Deps.Channels, static for a controller build (served with fieldToProto,
// like BackendService).
type channelHandler struct {
	syncrv1connect.UnimplementedChannelServiceHandler

	channels *notify.Registry
}

var _ syncrv1connect.ChannelServiceHandler = (*channelHandler)(nil)

// ListChannels returns every channel type, by label. How a channel sends
// is not part of its descriptor.
func (h *channelHandler) ListChannels(context.Context, *connect.Request[syncrv1.ListChannelsRequest]) (*connect.Response[syncrv1.ListChannelsResponse], error) {
	chs := h.channels.Channels()
	out := &syncrv1.ListChannelsResponse{Channels: make([]*syncrv1.ChannelType, 0, len(chs))}
	for _, c := range chs {
		out.Channels = append(out.Channels, channelToProto(c))
	}
	return connect.NewResponse(out), nil
}

func channelToProto(c notify.Channel) *syncrv1.ChannelType {
	fields := c.Fields()
	out := &syncrv1.ChannelType{
		Id:          c.ID(),
		Label:       c.Label(),
		Description: c.Description(),
		Fields:      make([]*syncrv1.BackendField, 0, len(fields)),
	}
	for _, f := range fields {
		out.Fields = append(out.Fields, fieldToProto(f))
	}
	return out
}
