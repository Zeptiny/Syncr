// Package passive is the controller's outbound side of the worker link for
// passive workers (docs/adr/0002-worker-link.md): a dispatch.PassiveClient
// that calls a worker's WorkerLinkService over HTTPS with a pinned
// certificate, and a Supervisor that keeps exactly one dispatch.PassiveLoop
// running per registered passive worker.
//
// # Certificate pinning
//
// Passive workers serve TLS with a self-signed certificate whose SHA-256
// fingerprint (of the leaf certificate's DER encoding, 64 hex digits) the
// admin registered. The Client trusts exactly that certificate:
//
//   - no CA verification and no host name verification (the worker's
//     address is often a bare IP, and its certificate is self-signed); the
//     pin replaces both, it is checked in VerifyConnection on every
//     handshake, including resumed sessions;
//   - a mismatch fails the handshake with an error wrapping
//     ErrFingerprintMismatch that names the presented fingerprint, so the
//     admin can compare it with the worker's;
//   - the system roots are never consulted, so a certificate that is valid
//     for some CA but not pinned is rejected too;
//   - the certificate's validity period is not checked: the pin is the
//     identity, and a worker rotates its certificate by re-registering;
//   - TLS 1.2 is the minimum (TLS 1.3 is negotiated when the worker offers
//     it, which Go always does); redirects are refused, and no proxy is
//     used.
//
// Every call sends link.PassiveAuthorization with the worker's secret, is
// bounded by ClientConfig.Timeout, and refuses responses larger than
// ClientConfig.ReadMaxBytes.
//
// # Supervision
//
// Supervisor.Run lists the passive workers every SupervisorOptions.Resync
// (and whenever SupervisorOptions.Wake fires) and reconciles the running
// loops with the list: it starts a loop for a new worker, stops the loop of
// a removed worker, and restarts the loop of a worker whose URL,
// fingerprint or secret changed (with a new client). A loop that returns or
// panics while it should run is restarted after an exponential backoff.
// When Run's context ends, every loop is stopped and waited for.
//
// Allowed dependencies: the standard library, connect, the generated code,
// internal/link, internal/domain, internal/app/workers (for its types) and
// internal/controller/dispatch. No adapters.
package passive
