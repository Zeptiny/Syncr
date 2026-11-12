package backend

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Kind is the value type of a field, which also selects its form widget:
// the generic descriptor kinds plus KindMembers. The string values are
// stable: they are exposed through the API.
//
// It is a defined type over descriptor.Kind rather than an alias, so that
// its Valid method can accept KindMembers, which stays backend-only.
type Kind descriptor.Kind

// Field kinds.
const (
	// KindString is single-line text.
	KindString = Kind(descriptor.KindString)
	// KindText is multi-line text (e.g. a PEM key).
	KindText = Kind(descriptor.KindText)
	// KindInt is a base-10 integer.
	KindInt = Kind(descriptor.KindInt)
	// KindBool is "true" or "false".
	KindBool = Kind(descriptor.KindBool)
	// KindEnum is one of the field's Choices.
	KindEnum = Kind(descriptor.KindEnum)
	// KindMembers marks where a union's member list goes in the form. It is
	// not a configuration value: members are managed by the remotes use
	// case, and CheckFields and MapFields ignore the field.
	KindMembers Kind = "members"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return descriptor.Kind(k).Valid() || k == KindMembers }

// NotRclone as a Field's Rclone name means the field is not an rclone
// option: the descriptor's ToRclone consumes it itself (e.g. a bucket or
// base path that becomes Rclone.Root).
const NotRclone = "-"

// Choice is one allowed value of an enum field.
type Choice = descriptor.Choice

// Field describes one configuration field of a backend type.
type Field struct {
	// Name is the key in Config.Options (or Config.Secrets for a secret
	// field): ^[a-z][a-z0-9_]{0,63}$, unique within the type.
	Name string
	// Label is the form label; Help and Placeholder are optional.
	Label       string
	Help        string
	Placeholder string
	// Default is the value MapFields uses when the field is left empty; ""
	// for none. It must parse as the field's kind; secret fields have none.
	Default string

	Kind Kind
	// Required fields must have a non-empty value. A required field is
	// never Advanced.
	Required bool
	// Secret fields are stored encrypted, never returned by the API, and
	// passed to rclone as secrets (never logged by the worker).
	Secret bool
	// Advanced fields are collapsed in the form.
	Advanced bool
	// Obscure marks a secret that rclone expects in its "obscured" form
	// (e.g. the sftp and webdav "pass"); the worker obscures it. Only for
	// secret fields.
	Obscure bool
	// Choices are the allowed values of an enum field.
	Choices []Choice

	// Rclone is the rclone option name the field maps to: "" means Name,
	// NotRclone means the field is not an rclone option.
	Rclone string
}

// Descriptor returns the generic part of f (without its rclone mapping),
// as the API serves it. A members field keeps its kind, "members".
func (f Field) Descriptor() descriptor.Field {
	return descriptor.Field{
		Name: f.Name, Label: f.Label, Help: f.Help, Placeholder: f.Placeholder, Default: f.Default,
		Kind: descriptor.Kind(f.Kind), Required: f.Required, Secret: f.Secret, Advanced: f.Advanced,
		Choices: slices.Clone(f.Choices),
	}
}

// valueFields returns the descriptors of fields without members fields
// (which are not configuration values).
func valueFields(fields []Field) []descriptor.Field {
	out := make([]descriptor.Field, 0, len(fields))
	for _, f := range fields {
		if f.Kind != KindMembers {
			out = append(out, f.Descriptor())
		}
	}
	return out
}

// RcloneName returns the rclone option the field maps to, or "" if it maps
// to none (NotRclone, or a members field).
func (f Field) RcloneName() string {
	switch {
	case f.Kind == KindMembers || f.Rclone == NotRclone:
		return ""
	case f.Rclone == "":
		return f.Name
	default:
		return f.Rclone
	}
}

// Capabilities say what remotes of a type can be used for.
type Capabilities struct {
	// Local remotes are paths on the worker's own filesystem (inside its
	// local_roots); their ToRclone returns an empty Backend and the root
	// path as Root.
	Local bool
	// Union remotes combine other remotes (their members). A union is never
	// local and never a union member itself.
	Union bool
	// UnionMember remotes may be members of a union.
	UnionMember bool
	// ResticRepository remotes may hold restic repositories.
	ResticRepository bool
	// ObjectStore remotes keep objects in buckets (S3, B2): a folder is
	// only a prefix of object names, so a folder that does not exist lists
	// as empty instead of failing. Messages about a location that holds
	// nothing say so. Never set on a local or union type.
	ObjectStore bool
}

// Config is a remote's configuration as the user entered it, keyed by field
// name: non-secret values in Options, secret values in Secrets. Union
// members are not part of it (see internal/app/remotes). Its Clone method
// returns a deep copy.
type Config = descriptor.Values

// Rclone is a typed rclone remote: what the controller sends to the worker
// as a RemoteDefinition (minus the alias, which the job builder assigns).
type Rclone struct {
	// Backend is the rclone backend type (e.g. "s3"). It is "" exactly when
	// the type is Local: the remote resolves to the worker's filesystem.
	Backend string
	// Options and Secrets are keyed by rclone option name, with values in
	// the form rclone parses for that option (e.g. sftp's key_pem escaped
	// onto one line), since the worker passes them on unchanged. Secrets
	// are not obscured; ObscureKeys lists the secrets rclone expects
	// obscured, which the worker obscures.
	Options     map[string]string
	Secrets     map[string]string
	ObscureKeys []string
	// Root is joined in front of every location path on the remote (with
	// JoinRemote, or JoinLocal for a Local type): e.g. the bucket, or a base
	// directory.
	Root string
}

// Clone returns a deep copy of r.
func (r Rclone) Clone() Rclone {
	r.Options = maps.Clone(r.Options)
	r.Secrets = maps.Clone(r.Secrets)
	r.ObscureKeys = slices.Clone(r.ObscureKeys)
	return r
}

// UnionMode is how a union may use one of its members. The string values are
// stable: they are stored.
type UnionMode string

// Union member modes.
const (
	// UnionReadWrite: files may be read, changed and created.
	UnionReadWrite UnionMode = "read_write"
	// UnionReadOnly: files are only read.
	UnionReadOnly UnionMode = "read_only"
	// UnionNoCreate: existing files may change, no new files are created.
	UnionNoCreate UnionMode = "no_create"
)

// Valid reports whether m is a known mode.
func (m UnionMode) Valid() bool {
	return m == UnionReadWrite || m == UnionReadOnly || m == UnionNoCreate
}

// Type is a backend type descriptor. Implementations are stateless values
// and safe for concurrent use.
type Type interface {
	// ID is the stable identifier stored with remotes:
	// ^[a-z][a-z0-9]{0,63}$, e.g. "s3".
	ID() string
	// Label and Description are shown in the type picker.
	Label() string
	Description() string
	// Fields lists the configuration fields in form order. The returned
	// slice is the caller's to keep.
	Fields() []Field
	Capabilities() Capabilities
	// Validate checks a configuration. It returns nil or a
	// *domain.ValidationError whose field paths are "options.<name>" or
	// "secrets.<name>"; messages never contain secret values.
	Validate(cfg Config) error
	// ToRclone maps a valid configuration to an rclone remote. It is
	// deterministic and only emits declared rclone option names; secret
	// values appear only in Rclone.Secrets.
	ToRclone(cfg Config) (Rclone, error)
}

// Exampler is implemented by types that provide example configurations,
// which the contract suite (internal/backend/backendtest) validates and maps.
// Examples should set every field.
type Exampler interface {
	Examples() []Config
}

// InvalidExample is a configuration a type's Validate must reject, for the
// contract suite (internal/backend/backendtest): it checks that Validate
// returns a *domain.ValidationError that names Field and contains no value
// of Config.
type InvalidExample struct {
	// Name says what is wrong with the configuration, for test failures.
	Name   string
	Config Config
	// Field is the field path the error must name, e.g.
	// "options.host_keys".
	Field string
}

// InvalidExampler is implemented by types that provide configurations
// their Validate must reject (see InvalidExample).
type InvalidExampler interface {
	InvalidExamples() []InvalidExample
}

// CheckFields checks cfg against fields (see descriptor.CheckFields): every
// key is a declared field, in the right map (Secrets for secret fields,
// Options otherwise), every required field has a non-empty value, and
// non-empty values parse as their kind (int, bool, enum choice; no NUL
// bytes; no line breaks in string fields). Members fields are ignored.
// Errors are ordered by field, then by unknown key; their messages never
// contain values.
func CheckFields(fields []Field, cfg Config) []domain.FieldError {
	return descriptor.CheckFields(valueFields(fields), cfg)
}

// MapFields maps the declared fields of cfg to rclone options and secrets,
// keyed by their rclone names. A field's value is its entry in cfg (Secrets
// for secret fields, Options otherwise) or, when that is empty, its
// Default; fields left without a value are omitted. Fields that are not
// rclone options (NotRclone, members) and keys that are not declared fields
// are ignored. Secret fields marked Obscure are listed in ObscureKeys, in
// field order. Backend and Root are left for the caller to set; Options and
// Secrets are never nil.
func MapFields(fields []Field, cfg Config) Rclone {
	out := Rclone{Options: map[string]string{}, Secrets: map[string]string{}}
	for _, f := range fields {
		name := f.RcloneName()
		if name == "" {
			continue
		}
		if f.Secret {
			if v := cfg.Secrets[f.Name]; v != "" {
				out.Secrets[name] = v
				if f.Obscure {
					out.ObscureKeys = append(out.ObscureKeys, name)
				}
			}
			continue
		}
		if v := cfg.Options[f.Name]; v != "" {
			out.Options[name] = v
		} else if f.Default != "" {
			out.Options[name] = f.Default
		}
	}
	return out
}

// MaxPathLen bounds the paths JoinRemote and JoinLocal accept, in bytes.
const MaxPathLen = 4096

// PathError reports a path JoinRemote or JoinLocal rejects. Its message
// says why, without the path, so callers can show it next to the field; it
// matches domain.ErrInvalidArgument.
type PathError struct {
	Reason string
}

func (e *PathError) Error() string { return e.Reason }

// Unwrap makes errors.Is(err, domain.ErrInvalidArgument) true.
func (e *PathError) Unwrap() error { return domain.ErrInvalidArgument }

// checkPaths rejects NUL and line breaks (they cannot be passed to rclone
// safely: union upstreams are a CSV list) and ".." segments, splitting at
// any of seps, in each of paths.
func checkPaths(seps string, paths ...string) error {
	for _, p := range paths {
		if len(p) > MaxPathLen {
			return errTooLong()
		}
		if strings.ContainsAny(p, "\x00\r\n") {
			return &PathError{Reason: "path must not contain NUL bytes or line breaks"}
		}
		for seg := range strings.FieldsFuncSeq(p, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
			if seg == ".." {
				return &PathError{Reason: `path must not contain ".." segments`}
			}
		}
	}
	return nil
}

func errTooLong() error {
	return &PathError{Reason: "path is longer than " + strconv.Itoa(MaxPathLen) + " bytes"}
}

// JoinRemote joins a remote's Root and a location path on it into the path
// rclone sees after "alias:". rel is relative to root even if it starts with
// "/"; with an empty root it is used as is (cleaned), so a leading "/" keeps
// its meaning for the backend (e.g. an absolute sftp path). "." segments and
// repeated or trailing slashes are removed; ".." segments, NUL bytes and
// line breaks are rejected (*PathError) in either part.
func JoinRemote(root, rel string) (string, error) {
	if err := checkPaths("/", root, rel); err != nil {
		return "", err
	}
	var joined string
	switch {
	case root == "" && rel == "":
		return "", nil
	case root == "":
		joined = cleanSlash(rel)
	case rel == "":
		joined = cleanSlash(root)
	default:
		joined = cleanSlash(root + "/" + rel)
	}
	if len(joined) > MaxPathLen {
		return "", errTooLong()
	}
	return joined, nil
}

// cleanSlash removes "." segments and empty segments from a "/"-separated
// path, keeping one leading "/" if p has one. p has no ".." segments.
func cleanSlash(p string) string {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	out := strings.Join(segs, "/")
	if strings.HasPrefix(p, "/") {
		out = "/" + out
	}
	return out
}

// JoinLocal joins a local remote's Root (an absolute directory on the
// worker) and a location path below it into an absolute local path. rel is
// relative to root even if it starts with a separator.
//
// When root looks like a Windows path (a drive letter such as "C:\" or
// "C:/", or a UNC path starting with `\\`) the result uses `\` and both "/"
// and `\` separate segments of rel; otherwise it is a POSIX path and only
// "/" separates segments. ".." segments, NUL bytes, line breaks and a root
// that is not absolute are rejected (*PathError). The worker still checks
// the result against its local_roots.
func JoinLocal(root, rel string) (string, error) {
	if windowsRoot(root) {
		if err := checkPaths(`/\`, root, rel); err != nil {
			return "", err
		}
		return joinWindows(root, rel)
	}
	if !strings.HasPrefix(root, "/") {
		return "", &PathError{Reason: "the root is not an absolute path"}
	}
	if err := checkPaths("/", root, rel); err != nil {
		return "", err
	}
	joined := cleanSlash(root + "/" + rel)
	if len(joined) > MaxPathLen {
		return "", errTooLong()
	}
	return joined, nil
}

// windowsRoot reports whether root is a Windows path: a drive letter
// followed by ":", or a UNC path.
func windowsRoot(root string) bool {
	if strings.HasPrefix(root, `\\`) {
		return true
	}
	return len(root) >= 2 && root[1] == ':' && ('a' <= root[0]|0x20 && root[0]|0x20 <= 'z')
}

// joinWindows joins a Windows root and a relative path with `\`. root must be
// a drive root path ("C:\...", "C:/...") or a UNC path (`\\server\share...`).
func joinWindows(root, rel string) (string, error) {
	isSep := func(r rune) bool { return r == '/' || r == '\\' }
	var prefix string
	var rest string
	switch {
	case strings.HasPrefix(root, `\\`):
		parts := strings.FieldsFunc(root, isSep)
		if len(parts) < 2 {
			return "", &PathError{Reason: `the root is not a complete UNC path (\\server\share)`}
		}
		prefix, rest = `\\`+parts[0]+`\`+parts[1], strings.Join(parts[2:], `\`)
	case len(root) >= 3 && isSep(rune(root[2])):
		prefix, rest = root[:2], root[3:]
	default:
		return "", &PathError{Reason: "the root is not an absolute path"}
	}
	var segs []string
	for _, part := range []string{rest, rel} {
		for s := range strings.FieldsFuncSeq(part, isSep) {
			if s != "." {
				segs = append(segs, s)
			}
		}
	}
	out := prefix + `\` + strings.Join(segs, `\`)
	if strings.HasPrefix(root, `\\`) && len(segs) == 0 {
		out = prefix
	}
	if len(out) > MaxPathLen {
		return "", errTooLong()
	}
	return out, nil
}
