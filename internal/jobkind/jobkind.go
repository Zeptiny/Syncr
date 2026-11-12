// Package jobkind turns job definitions into worker specs. A job definition
// (syncr.v1.JobDefinition) is what the API accepts and stores: an operation
// with locations on the owner's remotes, referred to by ID, without secrets.
// A Kind checks the definition when a job is created and, at dispatch time,
// builds the worker's syncr.v1.JobSpec from it and the resolved remotes and
// restic repositories (with their credentials, Resolved).
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
// Locate and LocateRepository (locate.go) build the same remote
// definitions and locations for a single path or repository; the
// controller's probes use them too.
//
// # Optional behavior (Milestone 5)
//
// A Kind may also implement:
//
//   - Keyed: the concurrency keys of its jobs (Refs.ConcurrencyKeys); jobs
//     sharing a key run one at a time, oldest first. Restic kinds use
//     ResticRepoKey.
//   - Unschedulable: its definitions cannot be scheduled (restores).
//   - SystemOnly: only the controller creates its jobs, never RunJob or a
//     schedule (restic_delete_data).
//   - Requirer: the link features a worker must report to run it.
//   - OSBound: the operating system a worker must run for a definition
//     (a backup with Windows VSS).
//   - Labeled: a short label for people ("Backup").
//   - Summarizer: display details of a finished job's output.
//
// The Registry answers these per kind name (Schedulable, UserStartable,
// RequiredFeatures, Label, Summarize) or definition (RequiredOS), with
// neutral answers for kinds that do not implement them. The worker
// requirements (Requirer, OSBound) apply wherever a worker is chosen or
// checked: the API and the scheduler refuse a chosen worker on another
// operating system, the scheduler picks a label's worker by both, and the
// dispatcher checks both, finally, when it sends the job.
//
// # Time limits (Milestone 6)
//
// A definition's max_runtime applies to every kind: Registry.Check checks
// its bounds (MinMaxRuntime to MaxMaxRuntime) and Registry.Describe adds it
// to the description (notifications); Registry.DescribeOperation leaves it
// out (job titles, next to the effective limit). Kinds do not see it: the dispatcher computes the
// effective limit (with the controller's default) and writes it into the
// spec.
//
// # Hooks
//
// A definition's hooks (JobDefinition.hooks) apply to every kind a user
// may start, too: Registry.Check checks their names and counts (hooks.go),
// and Registry.Build copies them into the spec. Kinds do not see them.
// Whether a worker defines the hooks a definition names is a requirement on
// the worker like its operating system: HookNames and MissingHooks answer
// it for the API, the scheduler and the dispatcher.
//
// The package is pure: no I/O, no logging. Specs contain secrets and exist
// only in memory.
//
// Allowed dependencies: the standard library, the generated protobuf code,
// google.golang.org/protobuf, internal/domain, internal/backend (the core
// API) and internal/link (feature names, and the names and limits of
// hooks).
package jobkind

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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

// Repository is a resolved restic repository a spec is built with: its
// location on one of the owner's remotes and its decrypted password. It
// exists only in memory, at dispatch (or probe) time.
type Repository struct {
	ID domain.ResticRepoID
	// RemoteID is the remote holding the repository; it must be in
	// Resolved.Remotes (with the members of a union).
	RemoteID domain.RemoteID
	// Path is the repository's path on the remote, relative to its root
	// ("" is the root).
	Path string
	// Password is the repository password (secret).
	Password string
	// ResticID is restic's repository config ID as the controller stored
	// it (64 hex digits); restic_delete_data verifies it before deleting.
	ResticID string
}

// Resolved is everything Build needs, with secrets: the remotes and the
// restic repositories of the definition's Refs.
type Resolved struct {
	// Remotes holds every remote of Refs.RemoteIDs, the remote of the
	// repository, and the members of the unions among them.
	Remotes map[domain.RemoteID]Remote
	// Repositories holds the repository of Refs.RepositoryID, if any.
	Repositories map[domain.ResticRepoID]Repository
}

// Names are the display names Describe uses. A missing entry is "Unknown
// remote" or "Unknown repository".
type Names struct {
	Remotes      map[domain.RemoteID]string
	Repositories map[domain.ResticRepoID]string
}

// Refs is what a definition refers to.
type Refs struct {
	// RemoteIDs are the remotes the definition uses, in reference order,
	// without duplicates (union members are not included: the remotes
	// service adds them when it resolves the remotes). A repository's
	// remote is not included either: the jobs service adds it.
	RemoteIDs []domain.RemoteID
	// SearchTerms are the texts a job list search matches (the mode and the
	// paths, as given); the jobs service adds the kind and the remote
	// names. They contain no secrets.
	SearchTerms []string
	// RepositoryID is the restic repository the definition uses; zero if
	// the kind uses none.
	RepositoryID domain.ResticRepoID
	// ConcurrencyKeys are the job's concurrency keys (Keyed), sorted and
	// without duplicates; nil if the kind has none. Registry.Check fills
	// them in.
	ConcurrencyKeys []string
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
	// Build returns the worker spec of a checked definition. in.Remotes
	// holds every remote of Check's Refs, the remote of its repository
	// and the members of the unions among them; in.Repositories holds the
	// repository of Refs.RepositoryID. Errors wrap
	// domain.ErrInvalidArgument (a definition or remote that cannot be
	// built, e.g. a path that a remote's root makes invalid, or a nested
	// union) or domain.ErrNotFound (a remote or repository it needs is
	// missing from in). Error messages never contain option or secret
	// values.
	Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error)
	// Describe returns a one-line description of the definition for
	// people, as the web UI's jobs list shows it, e.g. "Copy Photos → B2
	// offsite". names holds the display names of the remotes and
	// repositories it refers to; a missing one is "Unknown remote" or
	// "Unknown repository". It never fails: a definition it cannot
	// describe gets a generic description.
	Describe(def *syncrv1.JobDefinition, names Names) string
}

// Keyed is implemented by kinds whose jobs have concurrency keys: jobs that
// share a key are started one at a time, oldest first (by creation time,
// then ID). Keys are set once, when a job is created.
type Keyed interface {
	// ConcurrencyKeys returns the keys of a checked definition, each
	// matching ^[a-z0-9][a-z0-9:._-]{0,199}$; at most 8.
	ConcurrencyKeys(def *syncrv1.JobDefinition) []string
}

// Unschedulable is implemented by kinds whose definitions cannot be
// scheduled.
type Unschedulable interface {
	// Unschedulable returns why, for people, e.g. "restores are started
	// from the snapshot browser".
	Unschedulable() string
}

// SystemOnly is implemented by kinds whose jobs only the controller itself
// creates: RunJob and schedules never accept them.
type SystemOnly interface {
	// SystemOnly returns why, for people (a field error message on
	// "definition.<kind>"), e.g. "is started by deleting the repository".
	SystemOnly() string
}

// Requirer is implemented by kinds that need link features of the worker
// (internal/link Feature* names).
type Requirer interface {
	// RequiredFeatures returns the features a worker must report.
	RequiredFeatures() []string
}

// OSBound is implemented by kinds whose definitions may need a worker that
// runs a particular operating system.
type OSBound interface {
	// RequiredOS returns the requirement of a checked definition; the zero
	// value when any operating system will do.
	RequiredOS(def *syncrv1.JobDefinition) OSRequirement
}

// OSRequirement is a definition's requirement on the operating system of
// the worker that runs it (OSBound). The zero value requires nothing.
type OSRequirement struct {
	// OS is the operating system the worker must report, as Go names it
	// (GOOS, Hello.os), e.g. "windows"; empty when any will do.
	OS string
	// Field is the definition field that needs it, e.g.
	// "definition.restic_backup.use_fs_snapshot".
	Field string
	// What names that option for people, e.g. "Windows VSS
	// (use_fs_snapshot)".
	What string
}

// Allows reports whether a worker that reported operating system goos may
// run the definition. A worker that reported none ("") may not run one
// that requires an operating system; callers that only have a hint (no
// report yet) decide for themselves whether to let it pass.
func (r OSRequirement) Allows(goos string) bool { return r.OS == "" || goos == r.OS }

// FieldMessage is the field error (on Field) of a definition whose worker
// cannot meet r, e.g. "is only available on Windows workers".
func (r OSRequirement) FieldMessage() string {
	return "is only available on " + osName(r.OS) + " workers"
}

// Explain says, for people, what r requires, e.g. "Windows VSS
// (use_fs_snapshot) is only available on Windows workers".
func (r OSRequirement) Explain() string { return r.What + " " + r.FieldMessage() }

// osName returns the display name of an operating system Go names goos.
func osName(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	case "freebsd":
		return "FreeBSD"
	}
	return goos
}

// Labeled is implemented by kinds with a short label for people.
type Labeled interface {
	// Label returns the label, e.g. "Backup".
	Label() string
}

// Detail is one display detail of a finished job, e.g. {"Snapshot",
// "1a2b3c4d"}.
type Detail struct {
	Label string
	Value string
}

// Summarizer is implemented by kinds whose finished jobs have display
// details.
type Summarizer interface {
	// Summarize returns the details of a job of def with output out (nil
	// when the job has none or it cannot be decoded), in display order.
	Summarize(def *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail
}

// Builtin returns the built-in kinds: the rclone transfer (Transfer),
// followed by the restic kinds (ResticKinds).
func Builtin() []Kind { return append([]Kind{Transfer{}}, ResticKinds()...) }

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
// def refers to, with the concurrency keys of a Keyed kind (sorted, without
// duplicates). It also checks, for every kind, the definition's time limit
// (max_runtime: unset or zero, or between MinMaxRuntime and MaxMaxRuntime)
// and its hooks (checkHooks; a kind only the system starts takes none).
// Errors are *domain.ValidationError, with the fields of everything that is
// wrong.
func (r *Registry) Check(def *syncrv1.JobDefinition) (kind string, refs Refs, err error) {
	k, err := r.kindOf(def)
	if err != nil {
		return "", Refs{}, err
	}
	var common []domain.FieldError
	if e := checkMaxRuntime(def); e != nil {
		common = append(common, e.Fields...)
	}
	if e := checkHooks(def); e != nil {
		common = append(common, e.Fields...)
	} else if s, is := k.(SystemOnly); is && HooksOf(def) != nil {
		common = append(common, domain.FieldError{Field: hooksField, Message: "cannot be set: a job of this kind " + s.SystemOnly()})
	}
	refs, err = k.Check(def)
	if err != nil {
		var verr *domain.ValidationError
		if len(common) > 0 && errors.As(err, &verr) {
			err = &domain.ValidationError{Fields: append(slices.Clone(verr.Fields), common...)}
		}
		return "", Refs{}, err
	}
	if len(common) > 0 {
		return "", Refs{}, &domain.ValidationError{Fields: common}
	}
	refs.ConcurrencyKeys = nil
	if kk, ok := k.(Keyed); ok {
		if keys := kk.ConcurrencyKeys(def); len(keys) > 0 {
			keys = slices.Clone(keys)
			slices.Sort(keys)
			refs.ConcurrencyKeys = slices.Compact(keys)
		}
	}
	return k.Name(), refs, nil
}

// Build builds the worker spec of def with its kind (see Kind.Build) and
// gives it def's hooks (HooksOf; unset when def names none). Errors wrap
// domain.ErrInvalidArgument or domain.ErrNotFound.
func (r *Registry) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	k, err := r.kindOf(def)
	if err != nil {
		return nil, err
	}
	spec, err := k.Build(def, in)
	if err != nil {
		return nil, err
	}
	spec.Hooks = HooksOf(def)
	return spec, nil
}

// DescribeOperation describes what def does with its kind (see
// Kind.Describe), e.g. "Copy Photos → B2 offsite": the title of a job,
// whose page shows the effective time limit on its own. A definition of no
// registered kind is described by its kind name ("" if it has none).
func (r *Registry) DescribeOperation(def *syncrv1.JobDefinition, names Names) string {
	k, err := r.kindOf(def)
	if err != nil {
		return KindOf(def)
	}
	return k.Describe(def, names)
}

// Describe is DescribeOperation followed by def's own time limit, e.g.
// "Copy Photos → B2 offsite · Time limit 6h", for text that stands alone
// (notifications). The controller's default limit is not part of the
// definition, so it is not described.
func (r *Registry) Describe(def *syncrv1.JobDefinition, names Names) string {
	k, err := r.kindOf(def)
	if err != nil {
		return KindOf(def)
	}
	out := k.Describe(def, names)
	if limit, ok := maxRuntime(def); ok {
		out += " · Time limit " + formatDuration(limit)
	}
	return out
}

// Schedulable reports whether definitions of kind may be scheduled, and if
// not why (for people): an unregistered kind or an Unschedulable one.
// Schedules also reject kinds that are not UserStartable.
func (r *Registry) Schedulable(kind string) (ok bool, reason string) {
	k, found := r.kinds[kind]
	if !found {
		return false, fmt.Sprintf("job kind %q is not supported", kind)
	}
	if u, is := k.(Unschedulable); is {
		return false, u.Unschedulable()
	}
	return true, ""
}

// UserStartable reports whether users may start jobs of kind (RunJob and
// schedules), and if not why (for people): an unregistered kind or a
// SystemOnly one.
func (r *Registry) UserStartable(kind string) (ok bool, reason string) {
	k, found := r.kinds[kind]
	if !found {
		return false, fmt.Sprintf("job kind %q is not supported", kind)
	}
	if s, is := k.(SystemOnly); is {
		return false, s.SystemOnly()
	}
	return true, ""
}

// RequiredFeatures returns the link features a worker must report to run
// jobs of kind (Requirer), as a new slice; nil for kinds that need none
// and for unregistered ones.
func (r *Registry) RequiredFeatures(kind string) []string {
	if req, ok := r.kinds[kind].(Requirer); ok {
		return slices.Clone(req.RequiredFeatures())
	}
	return nil
}

// RequiredOS returns the operating system a worker must run to run def
// (OSBound); the zero OSRequirement for definitions of kinds without one,
// and for definitions of no registered kind.
func (r *Registry) RequiredOS(def *syncrv1.JobDefinition) OSRequirement {
	k, err := r.kindOf(def)
	if err != nil {
		return OSRequirement{}
	}
	if b, ok := k.(OSBound); ok {
		return b.RequiredOS(def)
	}
	return OSRequirement{}
}

// Label returns the label of kind (Labeled), else the kind name itself.
func (r *Registry) Label(kind string) string {
	if l, ok := r.kinds[kind].(Labeled); ok {
		if label := l.Label(); label != "" {
			return label
		}
	}
	return kind
}

// Summarize returns the display details of a finished job of kind with
// definition def and output, the encoded link JobOutput as jobs store it
// (protobuf binary; nil when the job has none). Output that cannot be
// decoded is passed to the kind as nil. Kinds without a Summarizer, and
// unregistered kinds, have no details (nil).
func (r *Registry) Summarize(kind string, def *syncrv1.JobDefinition, output []byte) []Detail {
	s, ok := r.kinds[kind].(Summarizer)
	if !ok {
		return nil
	}
	var out *syncrv1.JobOutput
	if len(output) > 0 {
		out = new(syncrv1.JobOutput)
		if err := proto.Unmarshal(output, out); err != nil {
			out = nil
		}
	}
	return s.Summarize(def, out)
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
