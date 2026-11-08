// Package notifications is the durable notification outbox (Milestone 4):
// it plans a notification for every subscriber of a finished job or of a
// missed or failed scheduled run, and delivers them through the channels
// (internal/notify) with retries.
//
// # Two stages
//
//   - Planning is exactly once and transactional. The planner scans the
//     source rows that are not notified yet (finished jobs, missed and
//     failed runs; Repository.Pending*), builds one Delivery per subscriber
//     whose events contain the outcome's event, and commits them with the
//     source's notified mark in one transaction (Repository.Commit*). A
//     crash anywhere only re-plans, and the dedupe keys (JobDedupeKey,
//     RunDedupeKey) stop duplicates. No job or schedule code takes part in
//     the transaction. Cancelled jobs never notify. Job history from before
//     the migration is marked notified.
//   - Delivery is at least once. Deliverers claim due deliveries with a
//     lease (Repository.ClaimDeliveries), resolve the contact, Send with the
//     message's ID set to the delivery ID (receivers deduplicate on it),
//     and complete the attempt with a compare-and-swap on the claimed
//     attempt count (Repository.CompleteDelivery).
//
// # Subscribers and events
//
//   - A scheduled job, and a missed or failed run, notify the enabled
//     contacts linked to the schedule whose link events contain the event
//     (failed, interrupted, succeeded, missed).
//   - A manual job notifies the owner's enabled contacts whose JobEvents
//     contain the event. A scheduled job never notifies through JobEvents.
//   - A disabled contact's already planned deliveries are still sent; a
//     deleted contact's deliveries are deleted with it.
//
// # Retries
//
// A temporary failure is retried after min(1h, 30s * 2^(attempt-1)) with
// jitter in [d/2, d], or after the target's RetryAfter (capped at 1h). A
// permanent failure, or Attempts reaching MaxAttempts, makes the delivery
// dead; Retry queues a dead one again with its attempts reset. Delivered
// and dead deliveries older than Config.Retention are purged.
//
// # Service (Milestone 4, track B)
//
// The Service is constructed as New(Deps{Repo Repository; Channels
// *notify.Registry; Contacts ContactResolver; Clock Clock; IDs IDGenerator;
// Random io.Reader; PublicURL string; Logger *slog.Logger}, Config). Its
// methods:
//
//	// Run runs the planner, the deliverers and the purge until ctx ends,
//	// every Config.Tick and whenever woken.
//	Run(ctx) error
//	// Wake asks the loops to run soon; it never blocks. The jobs service's
//	// Notifier and the scheduler call it.
//	Wake()
//	// One pass each, for tests: plan pending outcomes, deliver the due
//	// deliveries, purge old ones; each returns how many it handled.
//	Plan(ctx) (int, error)
//	DeliverDue(ctx) (int, error)
//	Purge(ctx) (int, error)
//	// List returns a page of the deliveries of actor's contacts matching f.
//	List(ctx, actor domain.Actor, f DeliveryFilter) (Page, error)
//	// Retry queues one of actor's dead deliveries again.
//	Retry(ctx, actor domain.Actor, id domain.DeliveryID) (Delivery, error)
//
// Allowed dependencies: the standard library, internal/domain,
// internal/descriptor and internal/notify; every other collaborator is a
// port declared in ports.go.
package notifications
