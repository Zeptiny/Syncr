package active

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// errTooManyEnrollments is returned to rate-limited clients.
var errTooManyEnrollments = errors.New("too many enrollment attempts; try again later")

// rateLimit is the request gate of EnrollmentService.
func (s *Server) rateLimit(ctx context.Context, spec connect.Spec, peer connect.Peer, header http.Header) (context.Context, error) {
	if key := s.clientKey(peer, header); !s.limiter.Allow(key) {
		s.log.WarnContext(ctx, "enrollment rate limit exceeded", "procedure", spec.Procedure, "client", key)
		return nil, connect.NewError(connect.CodeResourceExhausted, errTooManyEnrollments)
	}
	return ctx, nil
}

// enrollService implements EnrollmentService.
type enrollService struct {
	s *Server
}

var _ syncrv1connect.EnrollmentServiceHandler = (*enrollService)(nil)

// Enroll exchanges an enrollment token for worker credentials.
func (e *enrollService) Enroll(ctx context.Context, req *connect.Request[syncrv1.EnrollRequest]) (*connect.Response[syncrv1.EnrollResponse], error) {
	m := req.Msg
	en, err := e.s.workers.Enroll(ctx, workers.EnrollParams{
		Token:           m.GetEnrollmentToken(),
		Name:            m.GetName(),
		OS:              m.GetOs(),
		Arch:            m.GetArch(),
		Mode:            modeOf(m.GetMode()),
		URL:             m.GetUrl(),
		CertFingerprint: m.GetCertFingerprint(),
	})
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			e.s.log.WarnContext(ctx, "enrollment with an invalid, expired or used token", "peer", req.Peer().Addr)
			return nil, connect.NewError(connect.CodeUnauthenticated, errBadToken)
		}
		return nil, e.s.toConnect(ctx, syncrv1connect.EnrollmentServiceEnrollProcedure, err)
	}
	return connect.NewResponse(&syncrv1.EnrollResponse{WorkerId: en.Worker.ID.String(), WorkerSecret: en.Secret}), nil
}

// errBadToken is the only answer to a bad enrollment token, whatever the
// reason (unknown, expired or used).
var errBadToken = errors.New("invalid, expired or used enrollment token")

// modeOf maps the wire mode; unknown values stay invalid so the service
// rejects them.
func modeOf(m syncrv1.WorkerMode) domain.WorkerMode {
	switch m {
	case syncrv1.WorkerMode_WORKER_MODE_UNSPECIFIED:
		return ""
	case syncrv1.WorkerMode_WORKER_MODE_ACTIVE:
		return domain.WorkerActive
	case syncrv1.WorkerMode_WORKER_MODE_PASSIVE:
		return domain.WorkerPassive
	}
	return domain.WorkerMode(m.String())
}
