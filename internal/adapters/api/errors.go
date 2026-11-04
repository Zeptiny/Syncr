package api

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// errorMapper turns errors of the use cases into Connect errors. Domain
// sentinels map per the table in internal/domain/errors.go; anything else is
// logged and reported as a bare Internal error so that internals (SQL
// errors, file paths, decrypted values) never reach clients.
type errorMapper struct {
	log *slog.Logger
}

// toConnect maps err. what names the entity ("worker", "job") for the
// generic NotFound message; procedure is only logged.
func (m errorMapper) toConnect(ctx context.Context, procedure, what string, err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request timed out"))
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidID),
		errors.Is(err, domain.ErrInvalidJobStatus), errors.Is(err, domain.ErrInvalidWorkerMode):
		// Validation messages are written by the services for callers.
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, domain.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New(what+" not found"))
	case errors.Is(err, domain.ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, errors.New(what+" already exists"))
	case errors.Is(err, domain.ErrFailedPrecondition), errors.Is(err, domain.ErrInvalidTransition):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the "+what+" is not in a state that allows this"))
	case errors.Is(err, domain.ErrConflict):
		return connect.NewError(connect.CodeAborted, errors.New(what+" was modified concurrently; retry"))
	case errors.Is(err, domain.ErrUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	}
	m.log.ErrorContext(ctx, "api request failed", "procedure", procedure, "error", err.Error())
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// invalid returns an InvalidArgument error with message msg.
func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}
