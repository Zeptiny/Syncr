package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// SQLSTATE codes the store maps to domain errors
// (https://www.postgresql.org/docs/current/errcodes-appendix.html).
const (
	sqlstateNotNullViolation    = "23502"
	sqlstateForeignKeyViolation = "23503"
	sqlstateUniqueViolation     = "23505"
	sqlstateCheckViolation      = "23514"
)

// isBadValue reports data exceptions (class 22) caused by the value the
// caller supplied: a NUL byte or invalid UTF-8 in text, malformed text or
// numbers out of range.
func isBadValue(code string) bool {
	switch code {
	case "22001", "22003", "22021", "22025", "22P02", "22P05":
		return true
	}
	return false
}

// mapErr translates a database error into the domain sentinels the ports
// document, wrapped with op (the operation, e.g. "create job"):
//
//	unique violation                       ErrAlreadyExists
//	foreign key violation (on insert)      ErrNotFound (the referenced row is missing)
//	pgx.ErrNoRows                          ErrNotFound
//	check/not-null/bad-text/range errors   ErrInvalidArgument
//
// Methods whose "no row" result means a lost race (ErrConflict) intercept
// pgx.ErrNoRows before calling mapErr. Everything else (connection failures,
// context cancellation) is wrapped unchanged so callers can still match it
// with errors.Is, and counts as an internal error. The server's DETAIL is
// never included: it can echo row values.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlstateUniqueViolation:
			return fmt.Errorf("%s: %w (constraint %s)", op, domain.ErrAlreadyExists, pgErr.ConstraintName)
		case sqlstateForeignKeyViolation:
			return fmt.Errorf("%s: %w (constraint %s)", op, domain.ErrNotFound, pgErr.ConstraintName)
		case sqlstateCheckViolation, sqlstateNotNullViolation:
			return fmt.Errorf("%s: %w: violates %s", op, domain.ErrInvalidArgument, constraintOrColumn(pgErr))
		}
		if isBadValue(pgErr.Code) {
			return fmt.Errorf("%s: %w (sqlstate %s)", op, domain.ErrInvalidArgument, pgErr.Code)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

func constraintOrColumn(e *pgconn.PgError) string {
	if e.ConstraintName != "" {
		return "constraint " + e.ConstraintName
	}
	return "column " + e.ColumnName
}
