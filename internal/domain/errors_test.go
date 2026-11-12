package domain

import (
	"errors"
	"fmt"
	"testing"
)

// TestSentinelsAreDistinct: adapters map errors by errors.Is, so no
// sentinel may match another.
func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	all := []error{
		ErrInvalidID, ErrInvalidJobStatus, ErrInvalidTransition, ErrInvalidWorkerMode, ErrInvalidArgument,
		ErrNotFound, ErrAlreadyExists, ErrFailedPrecondition, ErrConflict, ErrUnauthenticated,
		ErrPermissionDenied, ErrRateLimited, ErrUnavailable,
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v matches %v", a, b)
			}
		}
	}
	wrapped := fmt.Errorf("worker %q did not answer in time: %w", "nas", ErrUnavailable)
	if !errors.Is(wrapped, ErrUnavailable) {
		t.Error("a wrapped ErrUnavailable does not match")
	}
}
