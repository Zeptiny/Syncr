package api

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// errorMapper turns errors of the use cases into Connect errors. Domain
// sentinels map per the table in internal/domain/errors.go; anything else is
// logged and reported as a bare Internal error so that internals (SQL
// errors, file paths, decrypted values) never reach clients.
//
// The messages of InvalidArgument and FailedPrecondition errors reach the
// caller: the use cases write them for people ("name is required", "the
// remote is a member of union \"Pool\"") and never put secrets or internals
// in them. A *domain.ValidationError also travels as a ValidationError
// detail, so forms can show each message next to its field. Other codes
// carry a generic message.
type errorMapper struct {
	log *slog.Logger
}

// toConnect maps err. what names the entity ("worker", "job") for the
// generic messages; procedure is only logged.
func (m errorMapper) toConnect(ctx context.Context, procedure, what string, err error) error {
	if err == nil {
		return nil
	}
	var (
		cerr *connect.Error
		verr *domain.ValidationError
	)
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request timed out"))
	case errors.As(err, &verr):
		return m.validation(ctx, procedure, err, verr.Fields)
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidID),
		errors.Is(err, domain.ErrInvalidJobStatus), errors.Is(err, domain.ErrInvalidWorkerMode):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, domain.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New(what+" not found"))
	case errors.Is(err, domain.ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, errors.New(what+" already exists"))
	case errors.Is(err, domain.ErrFailedPrecondition), errors.Is(err, domain.ErrInvalidTransition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, domain.ErrConflict):
		return connect.NewError(connect.CodeAborted, errors.New(what+" was modified concurrently; retry"))
	case errors.Is(err, domain.ErrUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credentials"))
	case errors.Is(err, domain.ErrPermissionDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
	case errors.Is(err, domain.ErrRateLimited):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many attempts; try again later"))
	}
	m.log.ErrorContext(ctx, "api request failed", "procedure", procedure, "error", err.Error())
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// validation returns an InvalidArgument error with message err and fields
// as a ValidationError detail.
func (m errorMapper) validation(ctx context.Context, procedure string, err error, fields []domain.FieldError) error {
	cerr := connect.NewError(connect.CodeInvalidArgument, err)
	detail, derr := connect.NewErrorDetail(validationErrorToProto(fields))
	if derr != nil {
		// Unreachable for a well-formed message; the error still says what
		// is wrong.
		m.log.ErrorContext(ctx, "cannot attach validation details", "procedure", procedure, "error", derr.Error())
		return cerr
	}
	cerr.AddDetail(detail)
	return cerr
}

func validationErrorToProto(fields []domain.FieldError) *syncrv1.ValidationError {
	out := &syncrv1.ValidationError{Violations: make([]*syncrv1.FieldViolation, 0, len(fields))}
	for _, f := range fields {
		out.Violations = append(out.Violations, &syncrv1.FieldViolation{Field: f.Field, Message: f.Message})
	}
	return out
}

// invalid returns an InvalidArgument error with message msg.
func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

// invalidField returns an InvalidArgument error about one request field,
// with a ValidationError detail naming it (e.g. invalidField("worker_id",
// "is required")).
func invalidField(field, msg string) error {
	verr := &domain.ValidationError{Fields: []domain.FieldError{{Field: field, Message: msg}}}
	cerr := connect.NewError(connect.CodeInvalidArgument, errors.New(field+" "+msg))
	if detail, err := connect.NewErrorDetail(validationErrorToProto(verr.Fields)); err == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}
