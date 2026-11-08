// Package contacts owns contacts: a user's configured notification targets
// of a channel type (internal/notify), with their credentials (Milestone
// 4). It follows the remotes pattern (internal/app/remotes).
//
// # Rules
//
//   - Contacts are strictly private to their owner, admins included:
//     another user's contact is ErrNotFound for every use case, including
//     as a schedule's contact.
//   - A name is 1 to 64 characters, unique per owner, case-insensitively.
//     The channel type never changes.
//   - Configurations are validated by the channel (notify.Channel.Validate)
//     on the merged result of an update, and the channel's Summary is
//     stored with the contact.
//   - Secrets are write-only. They are stored encrypted as one JSON object
//     (Cipher, associated data SecretsAD(id)) with their key names in
//     SecretKeys, and never returned or logged; the service never logs an
//     Input. On update a non-empty secret replaces the stored one, an absent
//     or empty one keeps it, ClearSecrets removes non-required ones, and
//     options are replaced as a whole.
//   - JobEvents (a subset of domain.JobEvents, empty by default) subscribe
//     the contact to the owner's manual jobs; scheduled jobs notify the
//     contacts linked to their schedule instead.
//   - Delete is a hard delete: it removes the contact's schedule links and
//     its delivery log.
//
// # Service (Milestone 4, track B)
//
// The Service is constructed as New(Deps{Contacts Repository; Channels
// *notify.Registry; Cipher Cipher; Limiter Limiter; Clock Clock; IDs
// IDGenerator; Logger *slog.Logger}, Config). Its methods:
//
//	// Create validates and stores a new contact of actor.
//	Create(ctx, actor domain.Actor, in Input) (Contact, error)
//	// Get returns one of actor's contacts.
//	Get(ctx, actor domain.Actor, id domain.ContactID) (Contact, error)
//	// List returns actor's contacts matching f (f.OwnerID is forced to the
//	// actor), by name.
//	List(ctx, actor domain.Actor, f ListFilter) ([]Contact, error)
//	// Update changes one of actor's contacts; version is the one the
//	// caller read (ErrConflict if it changed). The type must not change.
//	Update(ctx, actor domain.Actor, id domain.ContactID, version int64, in Input) (Contact, error)
//	// Delete deletes one of actor's contacts.
//	Delete(ctx, actor domain.Actor, id domain.ContactID) error
//	// UsedBy returns the schedules linked to each of actor's contacts ids.
//	UsedBy(ctx, actor domain.Actor, ids []domain.ContactID) (map[domain.ContactID][]ScheduleRef, error)
//	// SendTest sends a test notification synchronously (bounded to 15 s,
//	// rate limited per user: ErrRateLimited), with the stored contact id
//	// (zero for none) and/or unsaved form values in, merged over the
//	// stored secrets. A channel failure is a TestResult with OK false, not
//	// an error.
//	SendTest(ctx, actor domain.Actor, id domain.ContactID, in *Input) (TestResult, error)
//
//	// Usable returns the names of ids, which must all be contacts of
//	// owner (ErrNotFound otherwise): schedules.ContactChecker.
//	Usable(ctx, owner domain.UserID, ids []domain.ContactID) (map[domain.ContactID]string, error)
//	// Names returns display references of ids, of any owner.
//	Names(ctx, ids []domain.ContactID) (map[domain.ContactID]Ref, error)
//	// Resolve returns a contact with its decrypted configuration.
//	// ErrNotFound if it was deleted; ErrFailedPrecondition if its secrets
//	// cannot be decrypted.
//	Resolve(ctx, id domain.ContactID) (Resolved, error)
//	// ResolveChannel is Resolve as notifications.ContactResolver needs it
//	// (app packages cannot share types): the channel type and the
//	// decrypted configuration.
//	ResolveChannel(ctx, id domain.ContactID) (channelType string, cfg descriptor.Values, err error)
//
// Allowed dependencies: the standard library, internal/domain,
// internal/descriptor and internal/notify; every other collaborator is a
// port declared in ports.go.
package contacts
