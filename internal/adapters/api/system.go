package api

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/platform/version"
)

// SystemService implements syncr.v1.SystemService.
type SystemService struct {
	info version.Info
}

var _ syncrv1connect.SystemServiceHandler = (*SystemService)(nil)

// NewSystemService returns a SystemService reporting info.
func NewSystemService(info version.Info) *SystemService {
	return &SystemService{info: info}
}

// GetInfo returns the controller's build information.
func (s *SystemService) GetInfo(_ context.Context, _ *connect.Request[syncrv1.GetInfoRequest]) (*connect.Response[syncrv1.GetInfoResponse], error) {
	resp := &syncrv1.GetInfoResponse{
		Version: s.info.Version,
		Commit:  s.info.Commit,
	}
	if !s.info.BuildTime.IsZero() {
		resp.BuildTime = timestamppb.New(s.info.BuildTime)
	}
	return connect.NewResponse(resp), nil
}
