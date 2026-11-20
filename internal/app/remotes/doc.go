// Package remotes owns remotes: a user's configured storage locations of a
// backend type (internal/backend), with their credentials, union remotes
// made of other remotes (Milestone 3), and wrapper remotes (encrypted
// remotes, rclone crypt) that store their data in a folder of another
// remote.
//
// # Rules
//
//   - Remotes are strictly private to their owner, admins included: another
//     user's remote is ErrNotFound for every use case, including as a union
//     member, as a wrapped remote and as a job's remote.
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
//   - A wrapper type (backend.Capabilities.Wrapper, e.g. crypt) stores its
//     data in a folder of one other remote (Remote.Wrapped): a live remote
//     of the same owner, never itself, never local, a union or a wrapper
//     (no nesting), with a path that passes backend.JoinRemote with its
//     root. Other types take no Wrapped. A wrapper is never a union member
//     (its type has no UnionMember). Wrapped may change on update (to
//     follow data that was moved).
//   - Fixed fields (backend.Field.Fixed, e.g. a crypt password) keep the
//     value they were created with: Update and Draft of a stored remote
//     refuse another effective option value (the value, or the default
//     when empty), a new secret value or a clear, with FixedMessage.
//   - Delete is soft: job history keeps a valid reference and the name.
//     Deleting a remote that a live remote uses (a member of a live union,
//     the wrapped remote of a live wrapper) is ErrFailedPrecondition naming
//     it (InUseMessage). Queued jobs that use a deleted remote fail at
//     dispatch with "remote_unavailable".
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
//	// RemotesUsing returns the live remotes of actor that use remote id:
//	// unions that have it as a member, wrappers that store their data in
//	// it.
//	RemotesUsing(ctx, actor domain.Actor, id domain.RemoteID) ([]Remote, error)
//
//	// Usable returns the names of ids, which must all be live remotes of
//	// owner (ErrNotFound otherwise). The jobs service calls it when a job
//	// is created (jobs.RemoteChecker).
//	Usable(ctx, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error)
//	// Resolve returns ids, the members of the unions among them and the
//	// remotes the wrappers among them store their data in, decrypted and
//	// mapped to rclone remotes, with their types' capabilities, for the
//	// dispatcher building a job's spec. ErrNotFound if any is missing,
//	// deleted or foreign.
//	Resolve(ctx, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Resolved, error)
//	// Infos returns the type, wrapped remote and capabilities of ids,
//	// which must all be live remotes of owner (ErrNotFound otherwise),
//	// for the job definition checks that depend on them (jobkind's
//	// Registry.CheckWith).
//	Infos(ctx, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Info, error)
//	// Names returns display references of ids, of any owner, deleted ones
//	// included (missing ones are omitted), for job listings.
//	Names(ctx, ids []domain.RemoteID) (map[domain.RemoteID]Ref, error)
//
// Invalid input is reported as *domain.ValidationError with field paths
// "name", "type", "options.<field>", "secrets.<field>",
// "clear_secrets[<i>]", "members[<i>].<field>", "wrapped",
// "wrapped.remote_id" and "wrapped.path".
//
// Allowed dependencies: the standard library, internal/domain and
// internal/backend (the core API, not the built-in descriptors); every other
// collaborator is a port declared in ports.go.
package remotes
