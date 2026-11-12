// Package workers is the worker registry: enrollment of active workers,
// registration of passive ones, worker authentication, the per-worker link
// state the dispatcher maintains (epoch, ingest cursor, last contact) and the
// admin use cases (list, get, rename, remove).
//
// # Credentials
//
//   - Enrollment tokens are short-lived and single-use. Only their SHA-256
//     hash is stored; redeeming one and creating the worker it enrolls is
//     a single atomic repository call (TokenRepository.EnrollWorker), so a
//     failed create does not spend the token.
//   - Every worker has a link secret. Active workers receive a random one at
//     enrollment and present it to the controller; passive workers print a
//     pairing secret at setup that the admin registers, and the controller
//     presents it to the worker. Either way the controller must be able to
//     read it back, so it is stored encrypted (Cipher, associated data
//     "worker-secret:<worker id>"), never hashed. Authenticate decrypts and
//     compares in constant time.
//
// # Link state
//
// Worker.Link is owned by the dispatcher (internal/controller/dispatch) and
// changed only through conditional repository updates, so concurrent link
// calls of the same worker can never move it backwards:
//
//   - the epoch changes by compare-and-swap on the previous epoch
//     (AdvanceEpoch), keeping a bounded history of replaced epochs so a
//     delayed request from an old worker process can be recognized;
//   - the ingest cursor (AckSeq) only grows within its epoch;
//   - LastSeen only grows.
//
// Status (pending, online, offline) is derived from LastSeen and
// Config.OfflineAfter, never stored.
//
// # Removal
//
// Removal is soft: the worker keeps its row for job history, but its secret
// is wiped, it fails authentication, disappears from listings and its
// unfinished jobs are ended through the JobReleaser port.
//
// Allowed dependencies: the standard library and internal/domain; every
// other collaborator is a port declared in ports.go.
package workers
