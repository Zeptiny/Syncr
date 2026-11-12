package domain

import "strings"

// FieldError is one invalid input field.
type FieldError struct {
	// Field is the path of the field in the caller's input, e.g. "name",
	// "options.endpoint", "secrets.pass", "members[1].path".
	Field string
	// Message says what is wrong, for display. It never contains a secret
	// value.
	Message string
}

// ValidationError reports invalid input field by field, so that forms can
// show each message next to its field. It wraps ErrInvalidArgument: callers
// that only need the category match that; the API adapter attaches the
// fields to its InvalidArgument error as details.
//
// Services return it as a pointer and only with at least one field.
type ValidationError struct {
	Fields []FieldError
}

// Error lists the fields and their messages.
func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString(ErrInvalidArgument.Error())
	for i, f := range e.Fields {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		if f.Field != "" {
			b.WriteString(f.Field)
			b.WriteString(": ")
		}
		b.WriteString(f.Message)
	}
	return b.String()
}

// Unwrap makes errors.Is(err, ErrInvalidArgument) true.
func (e *ValidationError) Unwrap() error { return ErrInvalidArgument }

// Validation collects field errors. The zero value is ready to use.
//
//	var v domain.Validation
//	if name == "" {
//		v.Add("name", "is required")
//	}
//	return v.Err()
type Validation struct {
	fields []FieldError
}

// Add records that field is invalid.
func (v *Validation) Add(field, message string) {
	v.fields = append(v.fields, FieldError{Field: field, Message: message})
}

// AddAll records fields (e.g. from a nested validation), prefixing each
// field path with prefix ("" for none).
func (v *Validation) AddAll(prefix string, fields []FieldError) {
	for _, f := range fields {
		if prefix != "" {
			f.Field = joinFieldPath(prefix, f.Field)
		}
		v.fields = append(v.fields, f)
	}
}

// Fields returns the recorded field errors.
func (v *Validation) Fields() []FieldError { return v.fields }

// Err returns a *ValidationError with the recorded fields, or nil if there
// are none.
func (v *Validation) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: v.fields}
}

func joinFieldPath(prefix, field string) string {
	switch {
	case field == "":
		return prefix
	case strings.HasPrefix(field, "["):
		return prefix + field
	default:
		return prefix + "." + field
	}
}
