package domain

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestValidationError(t *testing.T) {
	t.Parallel()
	var v Validation
	if v.Err() != nil {
		t.Fatal("empty validation has an error")
	}
	v.Add("name", "is required")
	v.AddAll("members[1]", []FieldError{{Field: "path", Message: "must not contain .."}})
	v.AddAll("options", []FieldError{{Field: "endpoint", Message: "is not a URL"}, {Field: "[0]", Message: "x"}, {Message: "bad"}})
	err := v.Err()

	var verr *ValidationError
	if !errors.As(fmt.Errorf("create remote: %w", err), &verr) {
		t.Fatalf("errors.As failed for %v", err)
	}
	if !errors.Is(err, ErrInvalidArgument) {
		t.Error("ValidationError does not match ErrInvalidArgument")
	}
	want := []string{"name", "members[1].path", "options.endpoint", "options[0]", "options"}
	var got []string
	for _, f := range verr.Fields {
		got = append(got, f.Field)
	}
	if !slices.Equal(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
	if msg := err.Error(); msg != "invalid argument: name: is required; members[1].path: must not contain ..; "+
		"options.endpoint: is not a URL; options[0]: x; options: bad" {
		t.Errorf("Error() = %q", msg)
	}
	if got := (&ValidationError{Fields: []FieldError{{Message: "no field"}}}).Error(); got != "invalid argument: no field" {
		t.Errorf("Error() without field = %q", got)
	}
}
