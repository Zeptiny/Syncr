# ADR 0001: Ports and adapters, explicit extension points, one composition root

- Status: accepted
- Date: 2026-11-04

## Context

Syncr v1 (Django) mixed persistence, HTTP and rclone calls in the same
modules, which made it untestable and hard to extend (adding a remote type
touched models, forms, views and the rclone client). v2 must make new remote
types, job kinds, notifiers and transports cheap to add, and must stay
testable without Postgres or real tools.

## Decision

**Layers** (enforced by `depguard` rules in `.golangci.yml`):

| Layer | Packages | May import |
|---|---|---|
| Domain | `internal/domain` | standard library only |
| Use cases | `internal/app/...` | domain + the small port interfaces each package declares for what it consumes |
| Adapters | `internal/adapters/...` (API, Postgres, notifiers), worker transports and runners, tool providers | domain, app, platform, generated code; **never each other** |
| Shared link logic | `internal/link` | domain, generated code |
| Platform | `internal/platform/...` (config, logging, redaction, HTTP server, version) | standard library and well-known libraries; no Syncr business code |
| Composition roots | `cmd/syncr-controller`, `cmd/syncr-worker` | everything |

Controller and worker code never import each other; both may use
`internal/link`.

**Ports** are declared by the consumer (interface segregation and dependency
inversion): a use case defines e.g. `type JobStore interface { ... }` with
only the methods it needs, and the Postgres adapter satisfies it implicitly.

**Composition root.** Only `cmd/*` reads the process environment, constructs
adapters and wires them by constructor injection. There are no package-level
mutable variables, no `init()` side effects (`gochecknoglobals` and
`gochecknoinits` are enabled) and no global registries.

**Extension points** are registries filled explicitly in `cmd/`, e.g.
`runners.Register(rclone.New(...))`. Adding an implementation means one new
package and one registration line:

| Extension point | Contract | Lives in |
|---|---|---|
| Job kind (worker side) | `runner.Runner` | `internal/worker/runner/<kind>` |
| Job kind (controller side) | `jobkind.Kind` | Milestone 3 |
| Backend / repository type | `backend.Type` descriptor | `internal/backend/builtin/<type>` (Milestone 3) |
| Notification channel | `notify.Channel` | `internal/adapters/notify/<channel>` (Milestone 4) |
| Worker transport | `transport.Link` | `internal/worker/transport/{active,passive}` (Milestone 2) |
| Tool provisioning | `tools.Provider` chain | `internal/worker/tools` (Milestone 2) |
| Storage | per-aggregate repository ports | `internal/adapters/postgres` (Milestone 3) |

Job kinds are not declared separately: a kind is the name of the
`JobSpec.operation` field in `link.proto` (`link.KindOf`), so the controller
and worker agree by construction.

## Consequences

- Use cases are unit-tested with in-memory fakes of their ports.
- A depguard failure is a design signal: declare a port in the consumer and
  wire the implementation in `cmd/` instead of importing across layers.
- Some boilerplate (small interfaces, constructor parameters) is accepted in
  exchange for explicit dependencies.
