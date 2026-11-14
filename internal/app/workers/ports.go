package workers

import (
	"context"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Repository persists workers. Implementations must be safe for concurrent
// use. Every method is atomic on its own (one statement or one transaction
// inside the implementation); no use case needs a transaction spanning
// several calls. Errors wrap the domain sentinels named below; anything else
// is treated as an internal error. Returned values must not alias storage
// (slices are copied).
type Repository interface {
	// CreateWorker inserts w. ErrAlreadyExists if w.ID is taken or another
	// worker that is not removed has the same name, case-insensitively
	// (Postgres: the unique index workers_name_key).
	CreateWorker(ctx context.Context, w Worker) error

	// GetWorker returns the worker, including a removed one (RemovedAt set,
	// so job history can still show its name). ErrNotFound if it never
	// existed.
	GetWorker(ctx context.Context, id domain.WorkerID) (Worker, error)

	// ListWorkers returns the workers matching f in ID order (the service
	// sorts for display).
	ListWorkers(ctx context.Context, f ListFilter) ([]Worker, error)

	// RenameWorker sets the name of a worker that is not removed and returns
	// it. ErrNotFound if missing or removed; ErrAlreadyExists if another
	// worker that is not removed has the name, case-insensitively.
	RenameWorker(ctx context.Context, id domain.WorkerID, name string) (Worker, error)

	// SetWorkerLabels replaces the labels of a worker that is not removed
	// and returns it. labels are normalized by the caller
	// (domain.NormalizeLabels) and stored as given. ErrNotFound if missing
	// or removed.
	SetWorkerLabels(ctx context.Context, id domain.WorkerID, labels []string) (Worker, error)

	// RemoveWorker marks a worker removed: RemovedAt = at, SecretEnc =
	// nil. ErrNotFound if missing or already removed. Returns the removed
	// worker.
	RemoveWorker(ctx context.Context, id domain.WorkerID, at time.Time) (Worker, error)

	// The link-state methods below treat a removed worker as missing
	// (ErrNotFound). They are conditional updates; in SQL each is a single
	// UPDATE ... WHERE id = $1 AND removed_at IS NULL AND <condition>.

	// SetWorkerInfo replaces Info (all of it, Info.Epoch included, as
	// given) if Link.Epoch == epoch, so a delayed Hello of a replaced worker
	// process cannot overwrite a newer one. ErrConflict if the epoch
	// differs.
	SetWorkerInfo(ctx context.Context, id domain.WorkerID, epoch string, info Info) error

	// AdvanceWorkerEpoch is a compare-and-swap on the epoch: if Link.Epoch ==
	// from, it sets Link.Epoch = to.Epoch, Link.EpochStartedAt =
	// to.StartedAt, Link.PastEpochs = to.PastEpochs and Link.AckSeq = 0
	// (LastSeen is untouched) and returns the updated worker. ErrConflict if
	// Link.Epoch != from (another request advanced it first; re-read).
	AdvanceWorkerEpoch(ctx context.Context, id domain.WorkerID, from string, to NewEpoch) (Worker, error)

	// AdvanceWorkerAckSeq sets Link.AckSeq = max(Link.AckSeq, seq) if
	// Link.Epoch == epoch and returns the resulting AckSeq (which may be
	// higher than seq). ErrConflict if the epoch differs. The cursor never
	// moves backwards, whatever the order of concurrent calls.
	AdvanceWorkerAckSeq(ctx context.Context, id domain.WorkerID, epoch string, seq int64) (int64, error)

	// TouchWorker sets Link.LastSeen = max(Link.LastSeen, at).
	TouchWorker(ctx context.Context, id domain.WorkerID, at time.Time) error
}

// ListFilter selects workers for ListWorkers.
type ListFilter struct {
	// Mode keeps only workers of this mode; empty means any.
	Mode domain.WorkerMode
	// IncludeRemoved also returns removed workers.
	IncludeRemoved bool
	// Label keeps only workers with this label; empty means any.
	Label string
}

// NewEpoch is the link state a worker enters when its epoch changes.
type NewEpoch struct {
	Epoch      string
	StartedAt  time.Time
	PastEpochs []string
}

// TokenRepository persists enrollment tokens. Same rules as Repository.
type TokenRepository interface {
	// CreateEnrollmentToken inserts t. ErrAlreadyExists if its ID or hash
	// exists.
	CreateEnrollmentToken(ctx context.Context, t EnrollmentToken) error

	// GetEnrollmentToken returns the token with this ID, used or not,
	// until DeleteEnrollmentTokens deletes it. ErrNotFound otherwise.
	GetEnrollmentToken(ctx context.Context, id domain.EnrollmentTokenID) (EnrollmentToken, error)

	// EnrollWorker redeems a token and creates the worker it enrolls, as one
	// atomic step. If a token with this hash exists, is unused and at <
	// ExpiresAt, it sets UsedAt = at and UsedBy = w.ID, inserts w (with the
	// rules of Repository.CreateWorker) and returns the updated token.
	// Otherwise ErrNotFound, whether the token is unknown, used or expired
	// (callers must not be able to tell), and whatever w is: the token is
	// checked first. If w cannot be inserted (ErrAlreadyExists for a taken ID
	// or live name, or any other error) nothing changes and the token stays
	// redeemable. Of concurrent calls for the same token at most one
	// succeeds, and one that fails does not spend the token for the others
	// (SQL: one transaction of UPDATE ... WHERE hash = $1 AND used_at IS NULL
	// AND expires_at > $2 RETURNING ..., whose row lock serializes the
	// redemptions, and the INSERT of the worker).
	EnrollWorker(ctx context.Context, hash TokenHash, at time.Time, w Worker) (EnrollmentToken, error)

	// DeleteEnrollmentTokens deletes every token (used or not) with
	// ExpiresAt < before and returns how many it deleted.
	DeleteEnrollmentTokens(ctx context.Context, before time.Time) (int, error)
}

// Cipher encrypts secrets at rest; associated data binds a ciphertext to its
// record. internal/adapters/secrets implements it.
type Cipher interface {
	Encrypt(plaintext, associatedData []byte) ([]byte, error)
	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// IDGenerator mints worker and enrollment token IDs.
type IDGenerator interface {
	NewWorkerID() domain.WorkerID
	NewEnrollmentTokenID() domain.EnrollmentTokenID
}

// JobReleaser ends the unfinished jobs of a removed worker (implemented by
// the jobs service). It must be idempotent: Remove calls it again when
// retried.
type JobReleaser interface {
	ReleaseWorkerJobs(ctx context.Context, id domain.WorkerID) error
}
