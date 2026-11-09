package workers

import (
	"slices"
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Worker is a registered worker.
type Worker struct {
	ID   domain.WorkerID
	Name string
	Mode domain.WorkerMode

	// URL is the https:// base URL of a passive worker's WorkerLinkService;
	// empty for active workers.
	URL string
	// CertFingerprint pins a passive worker's TLS certificate: the SHA-256
	// of its DER encoding as 64 lower-case hex digits; empty for active
	// workers.
	CertFingerprint string
	// SecretEnc is the link secret encrypted with the Cipher (associated
	// data SecretAD(ID)); nil once removed.
	SecretEnc []byte

	CreatedAt time.Time
	// RemovedAt is set when the worker was removed; zero otherwise.
	RemovedAt time.Time

	// Labels are set by an admin (never reported by the worker), normalized
	// (domain.NormalizeLabels): lower case, sorted, without duplicates, at
	// most domain.MaxLabels. Schedules may target any worker with a label.
	Labels []string

	// Info is what the worker reported in its latest Hello; zero before the
	// first one.
	Info Info
	// Link is the dispatcher's state of the worker link.
	Link Link
}

// Removed reports whether the worker has been removed.
func (w Worker) Removed() bool { return !w.RemovedAt.IsZero() }

// Info is what a worker reports about itself in its Hello event.
type Info struct {
	Version           string
	OS                string
	Arch              string
	Hostname          string
	Tools             []Tool
	JobKinds          []string
	MaxConcurrentJobs int
	LocalRoots        []string
	// Features are the link features the worker implements (internal/link
	// Feature*, Hello.features); nil for a worker older than Milestone 5.
	Features []string
	// ProbeOps are the probe operations it can run (Hello.probe_ops).
	ProbeOps []string
	// ReportedAt is when the worker sent the Hello (worker clock).
	ReportedAt time.Time
}

// HasFeatures reports whether the worker reported every one of features.
func (i Info) HasFeatures(features ...string) bool {
	for _, f := range features {
		if !slices.Contains(i.Features, f) {
			return false
		}
	}
	return true
}

// Tool is a supervised tool and its version (rclone, restic).
type Tool struct {
	Name    string
	Version string
}

// Link is the state of a worker's link as seen by the dispatcher.
type Link struct {
	// Epoch identifies the worker process the controller currently talks
	// to; empty until the first link call.
	Epoch string
	// EpochStartedAt is when the controller first saw Epoch.
	EpochStartedAt time.Time
	// AckSeq is the highest event seq of Epoch whose effects are durably
	// stored; the worker may drop events up to it.
	AckSeq int64
	// PastEpochs are replaced epochs, most recent first, at most
	// MaxPastEpochs. A request carrying one of them is stale.
	PastEpochs []string
	// LastSeen is the time of the last authenticated link call; zero if
	// never.
	LastSeen time.Time
}

// MaxPastEpochs bounds Link.PastEpochs.
const MaxPastEpochs = 16

// EnrollmentToken is a stored enrollment token. The token itself is never
// stored, only its hash.
type EnrollmentToken struct {
	Hash      TokenHash
	CreatedAt time.Time
	ExpiresAt time.Time
	// UsedAt and UsedBy are set when the token was redeemed.
	UsedAt time.Time
	UsedBy domain.WorkerID
}

// TokenHash is the SHA-256 of an enrollment token string.
type TokenHash [32]byte

// EnrollmentGrant is a newly created enrollment token, returned once.
type EnrollmentGrant struct {
	Token     string
	ExpiresAt time.Time
}

// EnrollParams are the inputs of Enroll.
type EnrollParams struct {
	Token string
	// Name may be empty: the service then picks "worker-<id suffix>".
	Name string
	OS   string
	Arch string
	// Mode defaults to active when empty.
	Mode domain.WorkerMode
	// URL and CertFingerprint are required for passive workers and must be
	// empty for active ones.
	URL             string
	CertFingerprint string
}

// Enrollment is the result of Enroll: the new worker and its secret, which
// is returned this one time.
type Enrollment struct {
	Worker Worker
	Secret string
}

// PassiveParams are the inputs of RegisterPassive.
type PassiveParams struct {
	Name            string
	URL             string
	CertFingerprint string
	// Secret is the pairing secret the worker printed at setup.
	Secret string
}

// PassiveTarget is what a passive transport needs to call a worker.
type PassiveTarget struct {
	WorkerID        domain.WorkerID
	URL             string
	CertFingerprint string
	Secret          string
}
