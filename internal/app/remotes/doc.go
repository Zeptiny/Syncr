// Package remotes owns remotes: a user's configured storage locations of a
// backend type (internal/backend), with their credentials, and union remotes
// made of other remotes (Milestone 3).
//
// # Rules
//
//   - Remotes are strictly private to their owner, admins included: another
//     user's remote is ErrNotFound for every use case, including as a union
//     member and as a job's remote.
//   - A name is 1 to 64 characters, unique per owner among live remotes,
//     case-insensitively. The backend type never changes, so a member can
//     never later become a union.
//   - Configurations are validated by the type (backend.Type.Validate) on
//     the merged result of an update.
//   - Secrets are write-only. They are stored encrypted as one JSON object
//     (Cipher, associated data SecretsAD(id)) with their key names in
//     SecretKeys, and never returned or logged; the service never logs an
//     Input. On update a non-empty secret replaces the stored one, an absent
//     or empty one keeps it, ClearSecrets removes non-required ones, and
//     options are replaced as a whole.
//   - Union members are live remotes of the same owner whose type has
//     Capabilities.UnionMember, never the union itself and never a union; a
//     union has at least two. Member paths must pass backend.JoinRemote or
//     backend.JoinLocal. Unions cannot nest (matching the worker), so cycles
//     cannot occur; Resolve still guards with a visited set and depth 1.
//   - Delete is soft: job history keeps a valid reference and the name.
//     Deleting a member of a live union is ErrFailedPrecondition naming the
//     union. Queued jobs that use a deleted remote fail at dispatch with
//     "remote_unavailable".
//
// # Service (Milestone 3, track B)
//
// The Service is constructed as New(Deps, Config), with Deps.Backends a
// *backend.Registry plus the ports in ports.go. Its methods:
//
//	// Create validates and stores a new remote of actor.
//	Create(ctx, actor domain.Actor, in Input) (Remote, error)
//	// Get returns one of actor's remotes (ErrNotFound for deleted or
//	// foreign ones).
//	Get(ctx, actor domain.Actor, id domain.RemoteID) (Remote, error)
//	// List returns actor's live remotes matching f (f.OwnerID is forced to
//	// the actor), by name.
//	List(ctx, actor domain.Actor, f ListFilter) ([]Remote, error)
//	// Update changes one of actor's remotes; version is the one the caller
//	// read (ErrConflict if it changed). The type must not change.
//	Update(ctx, actor domain.Actor, id domain.RemoteID, version int64, in Input) (Remote, error)
//	// Delete soft-deletes one of actor's remotes.
//	Delete(ctx, actor domain.Actor, id domain.RemoteID) error
//	// UnionsUsing returns the live unions of actor that use remote id.
//	UnionsUsing(ctx, actor domain.Actor, id domain.RemoteID) ([]Remote, error)
//
//	// Usable returns the names of ids, which must all be live remotes of
//	// owner (ErrNotFound otherwise). The jobs service calls it when a job
//	// is created (jobs.RemoteChecker).
//	Usable(ctx, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error)
//	// Resolve returns ids and the members of the unions among them,
//	// decrypted and mapped to rclone remotes, for the dispatcher building a
//	// job's spec. ErrNotFound if any is missing, deleted or foreign.
//	Resolve(ctx, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Resolved, error)
//	// Names returns display references of ids, of any owner, deleted ones
//	// included (missing ones are omitted), for job listings.
//	Names(ctx, ids []domain.RemoteID) (map[domain.RemoteID]Ref, error)
//
// Invalid input is reported as *domain.ValidationError with field paths
// "name", "type", "options.<field>", "secrets.<field>",
// "clear_secrets[<i>]" and "members[<i>].<field>".
//
// Allowed dependencies: the standard library, internal/domain and
// internal/backend (the core API, not the built-in descriptors); every other
// collaborator is a port declared in ports.go.
package remotes
