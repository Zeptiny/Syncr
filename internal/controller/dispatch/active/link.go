package active

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// linkService implements ControllerLinkService for authenticated workers.
type linkService struct {
	s *Server
}

var _ syncrv1connect.ControllerLinkServiceHandler = (*linkService)(nil)

// Report hands the request to the dispatcher.
func (l *linkService) Report(ctx context.Context, req *connect.Request[syncrv1.ReportRequest]) (*connect.Response[syncrv1.ReportResponse], error) {
	id, err := workerID(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := l.s.disp.Report(ctx, id, req.Msg)
	if err != nil {
		return nil, l.s.toConnect(ctx, syncrv1connect.ControllerLinkServiceReportProcedure, err)
	}
	return connect.NewResponse(resp), nil
}

// Poll waits for commands for up to the dispatcher's MaxWait. It ends with
// an empty response when the server's lifetime ends.
func (l *linkService) Poll(ctx context.Context, req *connect.Request[syncrv1.PollRequest]) (*connect.Response[syncrv1.PollResponse], error) {
	id, err := workerID(ctx)
	if err != nil {
		return nil, err
	}
	msg := req.Msg
	if maxWait := l.s.disp.MaxWait(); msg.GetWait() == nil || msg.GetWait().AsDuration() > maxWait {
		msg.Wait = durationpb.New(maxWait)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(l.s.lifetime, func() { cancel(errShuttingDown) })
	defer stop()

	resp, err := l.s.disp.Poll(ctx, id, msg)
	if err != nil {
		if errors.Is(context.Cause(ctx), errShuttingDown) {
			return connect.NewResponse(&syncrv1.PollResponse{}), nil
		}
		return nil, l.s.toConnect(ctx, syncrv1connect.ControllerLinkServicePollProcedure, err)
	}
	return connect.NewResponse(resp), nil
}

// errShuttingDown ends long polls when the server's lifetime ends.
var errShuttingDown = errors.New("controller shutting down")

// CompleteProbe would hand a probe result to the dispatcher
// (dispatch.Service.CompleteProbe through the Dispatcher interface).
//
// m5: Track B implements this; until then it answers Unimplemented, so a
// worker drops the result.
func (l *linkService) CompleteProbe(ctx context.Context, _ *connect.Request[syncrv1.CompleteProbeRequest]) (*connect.Response[syncrv1.CompleteProbeResponse], error) {
	if _, err := workerID(ctx); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("probes are not supported by this controller yet"))
}
