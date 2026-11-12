package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not found", domain.ErrNotFound, connect.CodeNotFound},
		{"already exists", domain.ErrAlreadyExists, connect.CodeAlreadyExists},
		{"invalid argument", domain.ErrInvalidArgument, connect.CodeInvalidArgument},
		{"invalid id", domain.ErrInvalidID, connect.CodeInvalidArgument},
		{"invalid job status", domain.ErrInvalidJobStatus, connect.CodeInvalidArgument},
		{"invalid worker mode", domain.ErrInvalidWorkerMode, connect.CodeInvalidArgument},
		{"failed precondition", domain.ErrFailedPrecondition, connect.CodeFailedPrecondition},
		{"invalid transition", domain.ErrInvalidTransition, connect.CodeFailedPrecondition},
		{"conflict", domain.ErrConflict, connect.CodeAborted},
		{"unauthenticated", domain.ErrUnauthenticated, connect.CodeUnauthenticated},
		{"permission denied", domain.ErrPermissionDenied, connect.CodePermissionDenied},
		{"rate limited", domain.ErrRateLimited, connect.CodeResourceExhausted},
		{"validation", &domain.ValidationError{Fields: []domain.FieldError{{Field: "name", Message: "is required"}}}, connect.CodeInvalidArgument},
		{"canceled", context.Canceled, connect.CodeCanceled},
		{"deadline", context.DeadlineExceeded, connect.CodeDeadlineExceeded},
		{"unknown", errors.New("boom"), connect.CodeInternal},
		{"connect error passes through", connect.NewError(connect.CodeResourceExhausted, errors.New("x")), connect.CodeResourceExhausted},
		{"wrapped", fmt.Errorf("a: %w", fmt.Errorf("b: %w", domain.ErrNotFound)), connect.CodeNotFound},
		{"joined", errors.Join(errors.New("x"), domain.ErrConflict), connect.CodeAborted},
		{"both validation sentinels", fmt.Errorf("%w: %w", domain.ErrInvalidArgument, domain.ErrInvalidWorkerMode), connect.CodeInvalidArgument},
	}
	m := errorMapper{log: logging.Discard()}
	for _, tt := range tests {
		got := m.toConnect(context.Background(), "/p", "thing", tt.err)
		if c := connect.CodeOf(got); c != tt.want {
			t.Errorf("%s: code = %v (%v), want %v", tt.name, c, got, tt.want)
		}
	}
	if m.toConnect(context.Background(), "/p", "thing", nil) != nil {
		t.Error("nil error mapped to non-nil")
	}
}

func TestInternalErrorsDoNotLeak(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	m := errorMapper{log: newTestLogger(logs)}
	secret := errors.New("pq: connection to 10.0.0.5 failed: password=hunter2")
	err := m.toConnect(context.Background(), "/syncr.v1.JobService/GetJob", "job", fmt.Errorf("get job: %w", secret))
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "pq") {
		t.Errorf("err = %v", err)
	}
	// The detail goes to the log, under the procedure.
	if out := logs.String(); !strings.Contains(out, "hunter2") && !strings.Contains(out, "connection to 10.0.0.5") || !strings.Contains(out, "JobService/GetJob") {
		t.Errorf("log = %q", out)
	}
}

func TestMessagesAreGeneric(t *testing.T) {
	t.Parallel()
	m := errorMapper{log: logging.Discard()}
	nf := m.toConnect(context.Background(), "/p", "worker", fmt.Errorf("get worker 0000: %w: db row 42", domain.ErrNotFound))
	if got := nf.Error(); got != "not_found: worker not found" {
		t.Errorf("not found message = %q", got)
	}
	inv := m.toConnect(context.Background(), "/p", "worker", fmt.Errorf("rename: %w: name is empty", domain.ErrInvalidArgument))
	if !strings.Contains(inv.Error(), "name is empty") {
		t.Errorf("validation message = %q", inv)
	}
}

// violations returns the ValidationError detail of err.
func violations(t *testing.T, err error) []*syncrv1.FieldViolation {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("%v is not a Connect error", err)
	}
	for _, d := range cerr.Details() {
		v, err := d.Value()
		if err != nil {
			t.Fatal(err)
		}
		if ve, ok := v.(*syncrv1.ValidationError); ok {
			return ve.GetViolations()
		}
	}
	return nil
}

func TestValidationErrorDetails(t *testing.T) {
	t.Parallel()
	m := errorMapper{log: logging.Discard()}
	verr := &domain.ValidationError{Fields: []domain.FieldError{
		{Field: "name", Message: "is required"},
		{Field: "members[1].path", Message: "must not contain \"..\" segments"},
	}}
	err := m.toConnect(context.Background(), "/p", "remote", fmt.Errorf("create remote: %w", verr))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "name: is required") {
		t.Fatalf("err = %v", err)
	}
	got := violations(t, err)
	if len(got) != 2 || got[0].GetField() != "name" || got[0].GetMessage() != "is required" || got[1].GetField() != "members[1].path" {
		t.Errorf("violations = %v", got)
	}
	// Plain invalid-argument errors carry no detail.
	if v := violations(t, m.toConnect(context.Background(), "/p", "x", domain.ErrInvalidArgument)); v != nil {
		t.Errorf("plain InvalidArgument has violations %v", v)
	}

	ferr := invalidField("worker_id", "is required")
	if connect.CodeOf(ferr) != connect.CodeInvalidArgument || !strings.Contains(ferr.Error(), "worker_id is required") {
		t.Errorf("invalidField = %v", ferr)
	}
	if v := violations(t, ferr); len(v) != 1 || v[0].GetField() != "worker_id" {
		t.Errorf("invalidField violations = %v", v)
	}
}

func TestPreconditionMessagesReachCallers(t *testing.T) {
	t.Parallel()
	m := errorMapper{log: logging.Discard()}
	err := m.toConnect(context.Background(), "/p", "remote", fmt.Errorf("delete remote: %w: it is a member of union \"Pool\"", domain.ErrFailedPrecondition))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `union "Pool"`) {
		t.Errorf("err = %v", err)
	}
	for _, sentinel := range []error{domain.ErrPermissionDenied, domain.ErrRateLimited} {
		err := m.toConnect(context.Background(), "/p", "user", fmt.Errorf("detail only for logs: %w", sentinel))
		if strings.Contains(err.Error(), "detail only for logs") {
			t.Errorf("%v leaks its detail: %v", sentinel, err)
		}
	}
}
