// Package config loads a binary's configuration struct from an optional YAML
// file and SYNCR_* environment variables, then validates it.
//
// Each binary owns its configuration type; this package only provides the
// loading mechanics. Precedence, lowest to highest:
//
//  1. defaults: the caller pre-fills the struct before calling Load;
//  2. the YAML file, decoded strictly (unknown keys are errors);
//  3. environment variables named by `env:"SYNCR_..."` struct tags;
//
// and finally the struct's Validate method runs.
//
// Allowed dependencies: the standard library and go.yaml.in/yaml/v3.
package config

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix is the prefix every configuration environment variable must use.
const EnvPrefix = "SYNCR_"

// PathEnv names the environment variable binaries read the config file path
// from when no --config flag is given.
const PathEnv = EnvPrefix + "CONFIG"

// Validator is implemented by configuration structs.
type Validator interface {
	Validate() error
}

// LookupEnv has the signature of os.LookupEnv; tests inject fakes.
type LookupEnv func(key string) (string, bool)

// Load populates dst, a pointer to a struct pre-filled with defaults, from the
// YAML file at path (skipped when path is empty), then from environment
// variables, and validates the result.
//
// Fields are overridden from the environment when tagged `env:"NAME"` and
// NAME is set (even to an empty string). Supported field types are string,
// bool, integers, floats, time.Duration, []string (comma-separated) and types
// implementing encoding.TextUnmarshaler. Untagged struct fields are walked
// recursively.
func Load(path string, lookupEnv LookupEnv, dst Validator) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: destination must be a non-nil pointer to a struct, got %T", dst)
	}
	if path != "" {
		if err := decodeFile(path, dst); err != nil {
			return err
		}
	}
	if lookupEnv != nil {
		if err := applyEnv(v.Elem(), lookupEnv); err != nil {
			return err
		}
	}
	if err := dst.Validate(); err != nil {
		return fmt.Errorf("config: invalid configuration: %w", err)
	}
	return nil
}

func decodeFile(path string, dst any) error {
	f, err := os.Open(path) //nolint:gosec // the path comes from the operator (flag or SYNCR_CONFIG)
	if err != nil {
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// applyEnv walks the struct and applies every tagged field whose variable is
// set. It reports all problems at once.
func applyEnv(v reflect.Value, lookupEnv LookupEnv) error {
	var errs []error
	walkFields(v, func(field reflect.Value, sf reflect.StructField) {
		name, ok := sf.Tag.Lookup("env")
		if !ok {
			return
		}
		if !strings.HasPrefix(name, EnvPrefix) || len(name) == len(EnvPrefix) {
			errs = append(errs, fmt.Errorf("config: field %s: env tag %q must start with %s", sf.Name, name, EnvPrefix))
			return
		}
		raw, set := lookupEnv(name)
		if !set {
			return
		}
		if err := setField(field, raw); err != nil {
			errs = append(errs, fmt.Errorf("config: %s: %w", name, err))
		}
	})
	return errors.Join(errs...)
}

// walkFields calls fn for every settable leaf field, descending into untagged
// nested structs.
func walkFields(v reflect.Value, fn func(reflect.Value, reflect.StructField)) {
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		field := v.Field(i)
		if _, tagged := sf.Tag.Lookup("env"); !tagged && field.Kind() == reflect.Struct && !isTextUnmarshaler(field) {
			walkFields(field, fn)
			continue
		}
		fn(field, sf)
	}
}

func isTextUnmarshaler(field reflect.Value) bool {
	return field.CanAddr() && field.Addr().Type().Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}

// setField parses raw into field according to its type.
func setField(field reflect.Value, raw string) error {
	if isTextUnmarshaler(field) {
		u, _ := field.Addr().Interface().(encoding.TextUnmarshaler)
		if err := u.UnmarshalText([]byte(raw)); err != nil {
			return fmt.Errorf("invalid value %q: %w", raw, err)
		}
		return nil
	}
	if field.Type() == reflect.TypeFor[time.Duration]() {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", raw, err)
		}
		field.SetInt(int64(d))
		return nil
	}
	if field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.String {
		list := splitList(raw)
		s := reflect.MakeSlice(field.Type(), len(list), len(list))
		for i, item := range list {
			s.Index(i).SetString(item)
		}
		field.Set(s)
		return nil
	}

	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid boolean %q", raw)
		}
		field.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		field.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid unsigned integer %q", raw)
		}
		field.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, field.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid number %q", raw)
		}
		field.SetFloat(f)
	default:
		return fmt.Errorf("unsupported field type %s", field.Type())
	}
	return nil
}

// splitList splits a comma-separated list, trimming spaces and dropping empty
// entries.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
