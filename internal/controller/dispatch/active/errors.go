package active

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// errInternal is what callers see of an internal error.
var errInternal = errors.New("internal error")

// toConnect maps a use-case error to a Connect error (see the table in
// internal/domain). Internal errors are logged and replaced by a fixed
// message; unauthenticated errors never carry details.
func (s *Server) toConnect(ctx context.Context, procedure string, err error) error {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return ce
	}
	code := codeOf(err)
	switch code {
	case connect.CodeUnauthenticated:
		return connect.NewError(code, errBadCredentials)
	case connect.CodeInternal:
		s.log.ErrorContext(ctx, "link call failed", "procedure", procedure, "error", err.Error())
		return connect.NewError(code, errInternal)
	case connect.CodeCanceled, connect.CodeDeadlineExceeded:
		return connect.NewError(code, err)
	}
	s.log.DebugContext(ctx, "link call rejected", "procedure", procedure, "code", code.String(), "error", err.Error())
	return connect.NewError(code, err)
}

// codeOf returns the Connect code of err.
func codeOf(err error) connect.Code {
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		return connect.CodeUnauthenticated
	case errors.Is(err, domain.ErrNotFound):
		return connect.CodeNotFound
	case errors.Is(err, domain.ErrAlreadyExists):
		return connect.CodeAlreadyExists
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidID),
		errors.Is(err, domain.ErrInvalidJobStatus), errors.Is(err, domain.ErrInvalidWorkerMode):
		return connect.CodeInvalidArgument
	case errors.Is(err, domain.ErrFailedPrecondition), errors.Is(err, domain.ErrInvalidTransition):
		return connect.CodeFailedPrecondition
	case errors.Is(err, domain.ErrConflict):
		return connect.CodeAborted
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded
	}
	return connect.CodeInternal
}
