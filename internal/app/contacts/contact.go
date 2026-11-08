package contacts

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Contact is a configured notification target: an instance of a channel
// type (notify.Channel) with its configuration.
type Contact struct {
	ID      domain.ContactID
	OwnerID domain.UserID
	// Name is 1 to 64 characters, unique per owner (case-insensitive).
	Name string
	// Type is the channel type ID (notify.Channel.ID); it never changes.
	Type string

	// Options are the non-secret configuration values, keyed by field
	// name.
	Options map[string]string
	// SecretsEnc is the JSON object of secret values keyed by field name,
	// encrypted with the Cipher (associated data SecretsAD(ID)); nil when
	// there are none.
	SecretsEnc []byte
	// SecretKeys are the names of the secret fields that have a value,
	// sorted. They are not secret: the API shows them as "set".
	SecretKeys []string
	// Summary is the channel's non-secret one-line description of the
	// configuration (notify.Channel.Summary), stored for lists.
	Summary string

	// Enabled contacts get new notifications; a disabled contact's
	// already planned deliveries are still sent.
	Enabled bool
	// JobEvents are the events of the owner's manual (not scheduled) jobs
	// the contact is notified of: a normalized subset of domain.JobEvents,
	// empty by default.
	JobEvents domain.EventSet

	CreatedAt time.Time
	UpdatedAt time.Time
	// Version increases with every stored change; it is the optimistic
	// concurrency token of Repository.UpdateContact. A created contact has
	// version 1.
	Version int64
}

// Input is the configuration of a contact to create, update or test, as the
// caller sent it. It contains secret values: never log it.
type Input struct {
	Name string
	// Type is the channel type ID; on update it must equal the contact's.
	Type    string
	Options map[string]string
	// Secrets are new secret values; on update an absent or empty value
	// keeps the stored one.
	Secrets map[string]string
	// ClearSecrets lists secret fields to remove on update (only fields
	// that are not required).
	ClearSecrets []string
	Enabled      bool
	JobEvents    domain.EventSet
}

// Ref names a contact for display (e.g. a schedule's contact links).
type Ref struct {
	ID   domain.ContactID
	Name string
	Type string
}

// ScheduleRef names a schedule linked to a contact (Contact.used_by).
type ScheduleRef struct {
	ID   domain.ScheduleID
	Name string
}

// Resolved is a contact ready to send to: its decrypted configuration. It
// exists only in memory, at send time.
type Resolved struct {
	Contact Contact
	Values  descriptor.Values
}

// TestResult is the outcome of SendTest.
type TestResult struct {
	OK bool
	// Reason says why it failed (notify.SendError.Reason, or a validation
	// summary); never contains secrets.
	Reason string
	// Status is the reply's protocol status, if any.
	Status  int
	Latency time.Duration
}

// ListFilter selects contacts.
type ListFilter struct {
	// OwnerID keeps only this user's contacts; zero means every owner (the
	// service always sets it to the actor).
	OwnerID domain.UserID
	// Type keeps only contacts of this channel type; "" means all.
	Type string
}

// Limits.
const (
	// MaxNameLen bounds a contact name, in characters.
	MaxNameLen = 64
)

// SecretsAD is the associated data binding a contact's encrypted secrets to
// its record.
func SecretsAD(id domain.ContactID) []byte { return []byte("contact-secrets:" + id.String()) }
