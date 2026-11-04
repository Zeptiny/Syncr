package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

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
