// Package descriptor is the generic form-descriptor machinery shared by the
// extension points whose instances users configure through forms: storage
// backend types (internal/backend) and notification channel types
// (internal/notify). A descriptor lists Fields; a configuration is a set of
// Values keyed by field name, non-secret ones in Options and secret ones in
// Secrets. The web UI renders one form component from any descriptor (the
// API serves them as syncr.v1.BackendField).
//
// CheckShape checks a descriptor's static shape when a registry is built;
// CheckFields checks a configuration against its fields in Validate
// implementations. Neither ever puts a value into an error.
//
// Allowed dependencies: the standard library and internal/domain.
package descriptor

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Kind is the value type of a field, which also selects its form widget.
// The string values are stable: they are exposed through the API.
type Kind string

// Field kinds.
const (
	// KindString is single-line text.
	KindString Kind = "string"
	// KindText is multi-line text (e.g. a PEM key).
	KindText Kind = "text"
	// KindInt is a base-10 integer.
	KindInt Kind = "int"
	// KindBool is "true" or "false".
	KindBool Kind = "bool"
	// KindEnum is one of the field's Choices.
	KindEnum Kind = "enum"
)

// Valid reports whether k is one of the value kinds above. Extension points
// may add their own kinds (CheckShape's extraKinds), which are not values.
func (k Kind) Valid() bool {
	switch k {
	case KindString, KindText, KindInt, KindBool, KindEnum:
		return true
	}
	return false
}

// Choice is one allowed value of an enum field.
type Choice struct {
	Value string
	Label string
}

// Field describes one configuration field.
type Field struct {
	// Name is the key in Values.Options (or Values.Secrets for a secret
	// field): ^[a-z][a-z0-9_]{0,63}$, unique within the descriptor.
	Name string
	// Label is the form label; Help and Placeholder are optional.
	Label       string
	Help        string
	Placeholder string
	// Default is the value used when the field is left empty; "" for none.
	// It must parse as the field's kind; secret fields have none.
	Default string

	Kind Kind
	// Required fields must have a non-empty value. A required field is
	// never Advanced.
	Required bool
	// Secret fields are stored encrypted and never returned by the API.
	Secret bool
	// Advanced fields are collapsed in the form.
	Advanced bool
	// Choices are the allowed values of an enum field.
	Choices []Choice
}

// Values is a configuration as the user entered it, keyed by field name:
// non-secret values in Options, secret values in Secrets.
type Values struct {
	Options map[string]string
	Secrets map[string]string
}

// Clone returns a deep copy of v (nil maps stay nil).
func (v Values) Clone() Values {
	return Values{Options: maps.Clone(v.Options), Secrets: maps.Clone(v.Secrets)}
}

// fieldRe matches field names.
var fieldRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// CheckFields checks v against fields: every key is a declared field, in
// the right map (Secrets for secret fields, Options otherwise), every
// required field has a non-empty value, and non-empty values parse as their
// kind (int, bool, enum choice; no NUL bytes; no line breaks in string
// fields). Errors are ordered by field, then by unknown key; their field
// paths are "options.<name>" or "secrets.<name>" and their messages never
// contain values.
func CheckFields(fields []Field, v Values) []domain.FieldError {
	var errs []domain.FieldError
	add := func(path, msg string) { errs = append(errs, domain.FieldError{Field: path, Message: msg}) }
	byName := make(map[string]Field, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
		value, path := v.Options[f.Name], "options."+f.Name
		if f.Secret {
			value, path = v.Secrets[f.Name], "secrets."+f.Name
		}
		if value == "" {
			if f.Required {
				add(path, "is required")
			}
			continue
		}
		if msg := CheckValue(f, value); msg != "" {
			add(path, msg)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(v.Options)) {
		switch f, ok := byName[key]; {
		case !ok:
			add("options."+key, "is not a known field")
		case f.Secret:
			add("options."+key, "is a secret; send it in secrets")
		}
	}
	for _, key := range slices.Sorted(maps.Keys(v.Secrets)) {
		switch f, ok := byName[key]; {
		case !ok:
			add("secrets."+key, "is not a known field")
		case !f.Secret:
			add("secrets."+key, "is not a secret; send it in options")
		}
	}
	return errs
}

// CheckValue checks a non-empty value against f's kind; it returns a
// message for display (without the value) or "". Kinds other than the value
// kinds are only checked for NUL bytes.
func CheckValue(f Field, value string) string {
	if strings.ContainsRune(value, 0) {
		return "must not contain NUL bytes"
	}
	switch f.Kind {
	case KindString:
		if strings.ContainsAny(value, "\r\n") {
			return "must be a single line"
		}
	case KindInt:
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return "must be a whole number"
		}
	case KindBool:
		if value != "true" && value != "false" {
			return `must be "true" or "false"`
		}
	case KindEnum:
		if !slices.ContainsFunc(f.Choices, func(c Choice) bool { return c.Value == value }) {
			return "must be one of the listed choices"
		}
	}
	return ""
}

// CheckShape checks the static shape of a descriptor's fields:
//
//   - names are unique and match ^[a-z][a-z0-9_]{0,63}$, labels are not
//     empty, and kinds are valid value kinds or one of extraKinds (which an
//     extension point handles itself, e.g. a union's members);
//   - an enum field has choices with unique, non-empty values and labels,
//     and only enum fields have choices;
//   - a default parses as its field's kind, and secret fields have none;
//   - no field is both Required and Advanced.
//
// It returns every problem found, joined; each names the field by index
// and name.
func CheckShape(fields []Field, extraKinds ...Kind) error {
	names := map[string]bool{}
	var errs []error
	for i, f := range fields {
		ferr := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("field %d (%q): %s", i, f.Name, fmt.Sprintf(format, args...)))
		}
		switch {
		case !fieldRe.MatchString(f.Name):
			ferr("name must match %s", fieldRe)
		case names[f.Name]:
			ferr("name is used twice")
		}
		names[f.Name] = true
		if f.Label == "" {
			ferr("label is empty")
		}
		if !f.Kind.Valid() && !slices.Contains(extraKinds, f.Kind) {
			ferr("unknown kind %q", f.Kind)
		}
		if f.Required && f.Advanced {
			ferr("a required field cannot be advanced")
		}
		if f.Secret && f.Default != "" {
			ferr("a secret field cannot have a default")
		}
		if err := checkChoices(f); err != nil {
			ferr("%v", err)
		}
		if f.Default != "" {
			if msg := CheckValue(f, f.Default); msg != "" {
				ferr("default %s", msg)
			}
		}
	}
	return errors.Join(errs...)
}

// checkChoices checks an enum field's choices (and that other kinds have
// none).
func checkChoices(f Field) error {
	if f.Kind != KindEnum {
		if len(f.Choices) > 0 {
			return errors.New("only enum fields have choices")
		}
		return nil
	}
	if len(f.Choices) == 0 {
		return errors.New("an enum field needs choices")
	}
	seen := map[string]bool{}
	for i, c := range f.Choices {
		switch {
		case c.Value == "" || c.Label == "":
			return errors.New("choice " + strconv.Itoa(i) + " has an empty value or label")
		case seen[c.Value]:
			return fmt.Errorf("choice value %q is used twice", c.Value)
		}
		seen[c.Value] = true
	}
	return nil
}
