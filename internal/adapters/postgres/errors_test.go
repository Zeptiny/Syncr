package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestMapErr(t *testing.T) {
	t.Parallel()
	pg := func(code string) error {
		return &pgconn.PgError{Code: code, ConstraintName: "some_constraint", Detail: "Key (hash)=(\\x0102) leaks"}
	}
	tests := []struct {
		name string
		in   error
		want error // matched with errors.Is; nil means "none of the domain errors"
	}{
		{"unique violation", pg("23505"), domain.ErrAlreadyExists},
		{"foreign key violation", pg("23503"), domain.ErrNotFound},
		{"no rows", pgx.ErrNoRows, domain.ErrNotFound},
		{"check violation", pg("23514"), domain.ErrInvalidArgument},
		{"not null violation", pg("23502"), domain.ErrInvalidArgument},
		{"NUL byte in text", pg("22021"), domain.ErrInvalidArgument},
		{"invalid text", pg("22P02"), domain.ErrInvalidArgument},
		{"wrapped unique violation", fmt.Errorf("exec: %w", pg("23505")), domain.ErrAlreadyExists},
		{"serialization failure", pg("40001"), nil},
		{"connection failure", errors.New("connection refused"), nil},
		{"canceled", context.Canceled, nil},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapErr("do thing", tt.in)
			if got == nil {
				t.Fatal("mapErr returned nil")
			}
			if !strings.HasPrefix(got.Error(), "do thing: ") {
				t.Errorf("error %q lacks the operation prefix", got)
			}
			if strings.Contains(got.Error(), "leaks") {
				t.Errorf("error %q includes the server's DETAIL", got)
			}
			for _, sentinel := range []error{
				domain.ErrAlreadyExists, domain.ErrNotFound, domain.ErrInvalidArgument, domain.ErrConflict,
			} {
				if is := errors.Is(got, sentinel); is != (tt.want != nil && errors.Is(tt.want, sentinel)) {
					t.Errorf("errors.Is(%q, %v) = %v", got, sentinel, is)
				}
			}
			if tt.want == nil && !errors.Is(got, tt.in) {
				t.Errorf("unmapped error %q lost its cause", got)
			}
		})
	}
	if mapErr("op", nil) != nil {
		t.Error("mapErr(nil) != nil")
	}
}
