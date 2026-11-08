// Package jobkind turns job definitions into worker specs. A job definition
// (syncr.v1.JobDefinition) is what the API accepts and stores: an operation
// with locations on the owner's remotes, referred to by ID, without secrets.
// A Kind checks the definition when a job is created and, at dispatch time,
// builds the worker's syncr.v1.JobSpec from it and the resolved remotes
// (with their credentials).
//
// Kind names equal the field names of the JobDefinition operation oneof,
// which equal the JobSpec operation names the worker runners use (e.g.
// "rclone_transfer"). Adding a job kind is a Kind here plus a worker runner.
//
// # Building specs
//
// Build assigns remote aliases "r1", "r2", ... in reference order, then the
// members of unions, so aliases are valid rclone aliases that never
// overlap. A remote whose Rclone.Backend is "" (a local remote) becomes a
// local Location with backend.JoinLocal(Root, path); others become a
// RemoteDefinition with backend.JoinRemote(Root, path); a union becomes a
// RemoteDefinition whose union_upstreams are its members' aliases or local
// paths.
//
// The package is pure: no I/O, no logging. Specs contain secrets and exist
// only in memory.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// google.golang.org/protobuf, internal/domain and internal/backend (the core
// API).
package jobkind

import (
	"bytes"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Remote is a resolved remote a spec is built with: its rclone remote, with
// decrypted secrets.
type Remote struct {
	ID     domain.RemoteID
	Rclone backend.Rclone
	// Members of a union, in order. Every member is also an entry of the
	// map passed to Build.
	Members []Member
}

// Member is one member of a union remote.
type Member struct {
	RemoteID domain.RemoteID
	// Path on the member, relative to its root.
	Path string
	Mode backend.UnionMode
}

// Refs is what a definition refers to.
type Refs struct {
	// RemoteIDs are the remotes the definition uses, in reference order,
	// without duplicates (union members are not included: the remotes
	// service adds them when it resolves the remotes).
	RemoteIDs []domain.RemoteID
	// SearchTerms are the texts a job list search matches (the mode and the
	// paths, as given); the jobs service adds the kind and the remote
	// names. They contain no secrets.
	SearchTerms []string
}

// Kind is one kind of job. Implementations are stateless values and safe
// for concurrent use.
type Kind interface {
	// Name is the kind's JobDefinition oneof field name, e.g.
	// "rclone_transfer".
	Name() string
	// Check validates the definition's shape and path syntax (not that the
	// remotes exist or belong to the owner: the jobs service checks that)
	// and returns what it refers to. Errors are *domain.ValidationError
	// with field paths below "definition", e.g.
	// "definition.rclone_transfer.source.path".
	Check(def *syncrv1.JobDefinition) (Refs, error)
	// Build returns the worker spec of a checked definition. remotes holds
	// every remote of Check's Refs plus the members of the unions among
	// them. Errors wrap domain.ErrInvalidArgument (a definition or remote
	// that cannot be built, e.g. a path that a remote's root makes
	// invalid, or a nested union) or domain.ErrNotFound (a remote it needs
	// is missing from remotes). Error messages never contain option or
	// secret values.
	Build(def *syncrv1.JobDefinition, remotes map[domain.RemoteID]Remote) (*syncrv1.JobSpec, error)
}

// Builtin returns the built-in kinds: the rclone transfer (Transfer).
func Builtin() []Kind { return []Kind{Transfer{}} }

// Registry is an immutable set of kinds, keyed by name. It is safe for
// concurrent use.
type Registry struct {
	kinds map[string]Kind
}

// NewRegistry returns a registry of kinds. It rejects nil kinds, duplicate
// names and names that are not a JobDefinition operation field.
func NewRegistry(kinds ...Kind) (*Registry, error) {
	r := &Registry{kinds: make(map[string]Kind, len(kinds))}
	ops := operationOneof().Fields()
	for i, k := range kinds {
		if k == nil {
			return nil, fmt.Errorf("job kind registry: kind %d is nil", i)
		}
		name := k.Name()
		if ops.ByName(protoreflect.Name(name)) == nil {
			return nil, fmt.Errorf("job kind registry: %q is not a JobDefinition operation", name)
		}
		if _, dup := r.kinds[name]; dup {
			return nil, fmt.Errorf("job kind registry: kind %q is registered twice", name)
		}
		r.kinds[name] = k
	}
	return r, nil
}

// Get returns the kind named name.
func (r *Registry) Get(name string) (Kind, bool) {
	k, ok := r.kinds[name]
	return k, ok
}

// Check validates def with its kind and returns the kind's name and what
// def refers to. Errors are *domain.ValidationError.
func (r *Registry) Check(def *syncrv1.JobDefinition) (kind string, refs Refs, err error) {
	k, err := r.kindOf(def)
	if err != nil {
		return "", Refs{}, err
	}
	refs, err = k.Check(def)
	if err != nil {
		return "", Refs{}, err
	}
	return k.Name(), refs, nil
}

// Build builds the worker spec of def with its kind (see Kind.Build).
// Errors wrap domain.ErrInvalidArgument or domain.ErrNotFound.
func (r *Registry) Build(def *syncrv1.JobDefinition, remotes map[domain.RemoteID]Remote) (*syncrv1.JobSpec, error) {
	k, err := r.kindOf(def)
	if err != nil {
		return nil, err
	}
	return k.Build(def, remotes)
}

// kindOf returns the registered kind of def's operation.
func (r *Registry) kindOf(def *syncrv1.JobDefinition) (Kind, error) {
	name := KindOf(def)
	if name == "" {
		return nil, invalid("definition.operation", "is required")
	}
	k, ok := r.kinds[name]
	if !ok {
		return nil, invalid("definition.operation", fmt.Sprintf("job kind %q is not supported", name))
	}
	return k, nil
}

// KindOf returns the name of def's operation (the kind), or "" if def is
// nil or has none.
func KindOf(def *syncrv1.JobDefinition) string {
	if def == nil {
		return ""
	}
	m := def.ProtoReflect()
	fd := m.WhichOneof(operationOneof())
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

func operationOneof() protoreflect.OneofDescriptor {
	return (&syncrv1.JobDefinition{}).ProtoReflect().Descriptor().Oneofs().ByName("operation")
}

func invalid(field, msg string) error {
	return &domain.ValidationError{Fields: []domain.FieldError{{Field: field, Message: msg}}}
}

// Marshal encodes def as stored with jobs: compact protojson with the proto
// field names.
func Marshal(def *syncrv1.JobDefinition) ([]byte, error) {
	if def == nil {
		return nil, fmt.Errorf("encode job definition: %w: nil definition", domain.ErrInvalidArgument)
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("encode job definition: %w", err)
	}
	// protojson's whitespace is deliberately unstable; compact it.
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return nil, fmt.Errorf("encode job definition: %w", err)
	}
	return out.Bytes(), nil
}

// Unmarshal decodes a stored definition (see Marshal). Unknown fields are an
// error. Errors wrap domain.ErrInvalidArgument.
func Unmarshal(b []byte) (*syncrv1.JobDefinition, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("decode job definition: %w: empty", domain.ErrInvalidArgument)
	}
	def := new(syncrv1.JobDefinition)
	if err := protojson.Unmarshal(b, def); err != nil {
		return nil, fmt.Errorf("decode job definition: %w: %w", domain.ErrInvalidArgument, err)
	}
	return def, nil
}
