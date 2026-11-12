// Package active is the controller's inbound side of the worker link
// (docs/adr/0002-worker-link.md): the Connect handlers of
// ControllerLinkService (Report, Poll), which active workers call, and of
// EnrollmentService (Enroll), which every new worker calls once. It sits
// under dispatch because it only moves bytes: authentication and request
// limits happen here, everything else in dispatch.Service and
// workers.Service.
//
// # Authentication
//
// ControllerLinkService calls are authenticated by a Connect request gate,
// before the request body is read: link.ParseActiveAuthorization, then
// domain.ParseWorkerID, then workers.Authenticate (constant-time secret
// comparison). Every failure is answered with the same Unauthenticated error
// ("invalid worker credentials"), whatever the reason; the reason is logged
// at debug level by the workers service. The authenticated worker ID is the
// only identity the handlers use; message bodies never name the worker.
//
// EnrollmentService needs no worker credentials (the enrollment token in the
// request is the credential). Its gate applies a per-client rate limit (the
// Limiter port; RateLimiter by default), and its requests are limited to
// EnrollReadMaxBytes.
//
// # Errors
//
// Domain errors map to Connect codes as documented in internal/domain:
// unauthenticated to Unauthenticated (fixed message), a stale epoch (a
// replaced worker process) to FailedPrecondition, invalid arguments to
// InvalidArgument, a lost race to Aborted, missing entities to NotFound.
// Anything else is Internal with the fixed message "internal error"; the
// cause is logged, never sent.
//
// # Long polls and server timeouts
//
// A Poll waits for commands for up to the dispatcher's MaxWait (25 s by
// default; a longer or unset PollRequest.wait is capped to it). It ends
// early when commands arrive, when the client goes away, and when
// Options.Lifetime ends; in the last case it is answered with an empty
// response, so the worker simply polls again (and finds the server gone).
//
// The handler extends the connection's read and write deadlines of a Poll
// to MaxWait + Options.PollGrace through http.ResponseController, so the
// server-wide http.Server timeouts cannot cut a legitimate long poll (a
// ReadTimeout shorter than the poll would otherwise cancel its context, and
// a WriteTimeout would drop its response). The composition root MUST:
//
//   - pass as Options.Lifetime the context whose end starts the server's
//     graceful shutdown (with internal/platform/httpserver.Serve: the ctx
//     given to Serve), so pending long polls return at once instead of
//     holding Shutdown for up to MaxWait;
//   - set http.Server.ReadHeaderTimeout (e.g. 10 s); ReadTimeout and
//     WriteTimeout may be set for the rest of the server (for example 60 s
//     each) or left at zero, since Polls override them per request;
//   - set http.Server.IdleTimeout above the workers' report interval (e.g.
//     120 s) so keep-alive connections are reused;
//   - give the HTTP shutdown at least a few seconds (Reports in flight);
//   - configure any reverse proxy in front of the controller with a read
//     (idle) timeout above MaxWait + PollGrace, and without buffering
//     limits below Options.ReadMaxBytes.
//
// # Limits
//
// Link requests are limited to Options.ReadMaxBytes (16 MiB by default,
// both the raw body and the decoded message): workers bound a Report to
// about 4 MiB of events and a single event far below that. Enrollment
// requests are limited to Options.EnrollReadMaxBytes (64 KiB).
//
// Allowed dependencies: the standard library, connect, the generated code,
// internal/link, internal/domain and the internal/app services it calls
// (through the ports in this package). No adapters.
package active
