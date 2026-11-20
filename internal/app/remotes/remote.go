package remotes

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Remote is a configured remote.
type Remote struct {
	ID      domain.RemoteID
	OwnerID domain.UserID
	// Name is 1 to 64 characters, unique per owner among live remotes
	// (case-insensitive).
	Name string
	// Type is the backend type ID (backend.Type.ID); it never changes.
	Type string

	// Options are the non-secret configuration values, keyed by field
	// name.
	Options map[string]string
	// SecretsEnc is the JSON object of secret values keyed by field name,
	// encrypted with the Cipher (associated data SecretsAD(ID)); nil when
	// there are none, and once the remote is deleted.
	SecretsEnc []byte
	// SecretKeys are the names of the secret fields that have a value,
	// sorted. They are not secret: the API shows them as "set".
	SecretKeys []string

	// Members of a union remote, in order; nil for other types.
	Members []Member
	// Wrapped is where a wrapper remote (backend.Capabilities.Wrapper, an
	// encrypted remote) stores its data; zero for other types.
	Wrapped Wrapped

	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt is when the remote was deleted; zero while it is live.
	DeletedAt time.Time

	// Version increases with every stored change; it is the optimistic
	// concurrency token of Repository.UpdateRemote. A created remote has
	// version 1.
	Version int64
}

// Deleted reports whether the remote has been deleted.
func (r Remote) Deleted() bool { return !r.DeletedAt.IsZero() }

// Member is one member of a union remote.
type Member struct {
	RemoteID domain.RemoteID
	// Path on the member remote, relative to its root; "" is its root.
	Path string
	Mode backend.UnionMode
}

// Wrapped is where a wrapper remote stores its data: a folder of another
// live remote of the same owner, which is never local, a union or a
// wrapper itself.
type Wrapped struct {
	RemoteID domain.RemoteID
	// Path on the wrapped remote, relative to its root; "" is its root.
	Path string
}

// IsZero reports whether w is unset: no remote and no path.
func (w Wrapped) IsZero() bool { return w.RemoteID.IsZero() && w.Path == "" }

// Input is the configuration of a remote to create or update, as the caller
// sent it. It contains secret values: never log it.
type Input struct {
	Name string
	// Type is the backend type ID; on update it must equal the remote's.
	Type    string
	Options map[string]string
	// Secrets are new secret values; on update an absent or empty value
	// keeps the stored one.
	Secrets map[string]string
	// ClearSecrets lists secret fields to remove on update (only fields
	// that are not required).
	ClearSecrets []string
	Members      []Member
	// Wrapped is where a wrapper remote stores its data (required for a
	// wrapper type, refused for any other); it may change on update.
	Wrapped Wrapped
}

// Resolved is a remote ready for a job: its rclone remote with decrypted
// secrets. It exists only in memory, at dispatch time.
type Resolved struct {
	ID     domain.RemoteID
	Name   string
	Type   string
	Rclone backend.Rclone
	// Members of a union, in order (each member is also in Resolve's
	// result).
	Members []Member
	// Wrapped is where a wrapper remote stores its data (the wrapped
	// remote is also in Resolve's result); zero for other types.
	Wrapped Wrapped
	// Capabilities are those of the remote's backend type (e.g.
	// NoChecksum, for the job builder).
	Capabilities backend.Capabilities
}

// Info is what the checks of a job definition need to know about one of
// its remotes (Infos), without credentials or options.
type Info struct {
	// Type is the backend type ID.
	Type string
	// Wrapped is where a wrapper remote stores its data; zero for other
	// types.
	Wrapped Wrapped
	// Capabilities are those of the backend type; zero when the type is no
	// longer available.
	Capabilities backend.Capabilities
}

// Ref names a remote for display, e.g. in a job's details. It may be
// another user's or a deleted remote.
type Ref struct {
	ID domain.RemoteID
	// OwnerID is the remote's owner.
	OwnerID domain.UserID
	Name    string
	Type    string
	Deleted bool
}

// ListFilter selects remotes for List.
type ListFilter struct {
	// OwnerID keeps only this user's remotes; zero means every owner (the
	// service always sets it to the actor).
	OwnerID domain.UserID
	// Type keeps only remotes of this backend type; "" means all.
	Type string
}

// SecretsAD is the associated data binding a remote's encrypted secrets to
// its record.
func SecretsAD(id domain.RemoteID) []byte { return []byte("remote-secrets:" + id.String()) }
